package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type s string

const (
	leader    s = "leader"
	follower  s = "follower"
	candidate s = "candidate"
)

var state = follower
var term = 0
var leaderId string

var KVs = map[string]string{}

type command struct {
	key string
	val string

	rsp chan error
}

var cLog = []command{}

var proposal = make(chan command)
var commitIdx = 0

type member struct {
	id   string
	addr string
}

type membersFlag []member

func (m *membersFlag) String() string {
	return fmt.Sprintf("%v", *m)
}

func (m *membersFlag) Set(value string) error {
	parts := strings.Split(value, ",")
	for _, part := range parts {
		subParts := strings.Split(part, ";")
		if len(subParts) != 2 {
			return fmt.Errorf("invalid member format, expected id:addr")
		}
		id := strings.TrimSpace(subParts[0])
		addr := strings.TrimSpace(subParts[1])
		if id == "" || addr == "" {
			continue
		}
		*m = append(*m, member{id: id, addr: addr})
	}
	return nil
}

var members membersFlag

var lastHeartbeat = time.Now()

var votedInTerm = map[int]bool{}
var stepDown = make(chan struct{})

var id string
var httpAddr string

func init() {
	flag.StringVar(&id, "id", "node0", "Set the node ID")
	flag.StringVar(&httpAddr, "addr", "localhost:8080", "Set the HTTP bind address")
	flag.Var(&members, "members", "Comma-separated list of members in the format id:addr")
}

type service struct{}

func (s *service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/append-entries" {
		if state != follower {
			log.Printf("Received request from %s, not a follower\n", r.RemoteAddr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		t := r.URL.Query().Get("term")
		if t == "" {
			log.Printf("Received request from %s, term not found\n", r.RemoteAddr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		termInt, err := strconv.Atoi(t)
		if err != nil {
			log.Printf("Received request from %s, term not a number\n", r.RemoteAddr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		id := r.URL.Query().Get("id")
		if id == "" {
			log.Printf("Received request from %s, id not found\n", r.RemoteAddr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		//log.Printf("Received append-entries request from %s, term %d\n", id, termInt)

		lastHeartbeat = time.Now()
		leaderId = id

		if termInt != term {
			term = termInt
			log.Printf("Updated term to %d\n", termInt)
		}

		// handle request

		key := r.URL.Query().Get("key")
		val := r.URL.Query().Get("val")

		if key != "" && val != "" {
			log.Printf("Received key %s with val %s\n", key, val)
			cLog = append(cLog, command{key: key, val: val})
		}

		commitIdxStr := r.URL.Query().Get("commitIdx")
		if commitIdxStr == "" {
			log.Printf("Commit index not found\n")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		idx, err := strconv.Atoi(commitIdxStr)
		if err != nil {
			log.Printf("Commit index not a number\n")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if idx > commitIdx {
			log.Printf("Commit index updated to %d\n", idx)
			commitIdx = idx
			cmd := cLog[commitIdx-1]
			KVs[cmd.key] = cmd.val
		}

	} else if r.URL.Path == "/request-vote" {
		t := r.URL.Query().Get("term")
		if t == "" {
			log.Printf("Received request vote request from %s, term not found\n", r.RemoteAddr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		termInt, err := strconv.Atoi(t)
		if err != nil {
			log.Printf("Received request vote request from %s, term not a number\n", r.RemoteAddr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		id := r.URL.Query().Get("id")
		if id == "" {
			log.Printf("Received request vote request from %s, id not found\n", r.RemoteAddr)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		//log.Printf("Received request from %s, term %d, id %s\n", r.RemoteAddr, termInt, id)

		if termInt < term {
			log.Printf("Term is less than current term, rejecting vote request\n")
			w.Write([]byte("false"))
			return
		} else {
			if termInt > term {
				term = termInt
				log.Printf("Updated term to %d\n", termInt)
			}
			if state != follower {
				stepDown <- struct{}{}
			}
			if votedInTerm[term] {
				log.Printf("Already voted in term %d, rejecting vote request\n", term)
				w.Write([]byte("F"))
				return
			}
			log.Printf("Voting for %s in term %d\n", id, term)
			votedInTerm[term] = true
			w.Write([]byte("T"))
			return
		}

	} else if r.URL.Path == "/set" {
		if state != leader {
			// forward request to leader
			log.Printf("Forwarding request to leader\n")
			if leaderId == "" {
				log.Printf("No leader found\n")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			leaderAddr := ""
			for _, m := range members {
				if m.id == leaderId {
					leaderAddr = m.addr
					break
				}
			}

			if leaderAddr == "" {
				log.Printf("Leader not found in members\n")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			key := r.URL.Query().Get("key")
			if key == "" {
				log.Printf("Key not found\n")
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			val := r.URL.Query().Get("val")
			if val == "" {
				log.Printf("Val not found\n")
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			url := fmt.Sprintf("http://%s/set?key=%s&val=%s", leaderAddr, key, val)
			log.Printf("Forwarding request to leader: %s\n", url)
			_, err := http.Get(url)
			if err != nil {
				log.Printf("Failed to forward request to leader: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			return
		}

		// handle request

		key := r.URL.Query().Get("key")
		if key == "" {
			log.Printf("Key not found\n")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		val := r.URL.Query().Get("val")
		if val == "" {
			log.Printf("Val not found\n")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		rsp := make(chan error)
		log.Printf("Proposing key %s with val %s\n", key, val)
		proposal <- command{key: key, val: val, rsp: rsp}
		log.Printf("Waiting for response\n")

		select {
		case err := <-rsp:
			if err != nil {
				log.Printf("Failed to set key %s: %v\n", key, err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		case <-time.After(1 * time.Second):
			log.Printf("Failed to set key %s: timeout\n", key)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else if r.URL.Path == "/get" {
		key := r.URL.Query().Get("key")
		if key == "" {
			log.Printf("Invalid key\n")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		val, ok := KVs[key]
		if !ok {
			log.Printf("Key not found\n")
			w.WriteHeader(http.StatusNotFound)
			return
		}

		w.Write([]byte(val))
		return
	} else {
		w.WriteHeader(http.StatusNotFound)
	}
}

func main() {
	flag.Parse()

	log.Printf("Starting node %s on %s", id, httpAddr)
	log.Printf("Members: %v", members)

	svc := &service{}

	server := http.Server{
		Handler: svc,
	}

	ln, err := net.Listen("tcp", httpAddr)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	go func() {
		log.Printf("Listening on %s", httpAddr)
		if err := server.Serve(ln); err != nil {
			log.Fatalf("failed to serve: %v", err)
		}
	}()

	go func() {
		for {
			switch state {
			case follower:
				runFollower()
			case candidate:
				runCandidate()
			case leader:
				runLeader()
			}
		}
	}()

	// Create a channel to receive OS signals
	terminate := make(chan os.Signal, 1)

	// Notify the channel when SIGINT is received
	signal.Notify(terminate, syscall.SIGINT)

	for {
		select {
		case <-terminate:
			log.Printf("Shutting down server...")
			ln.Close()
			os.Exit(0)
		}
	}
}

func runFollower() {
	log.Printf("Running as follower...")
	for {
		electionTimeout := time.Duration(200+rand.Intn(300)) * time.Millisecond
		// log.Printf("sleeping for %v", electionTimeout)

		select {
		case <-time.After(electionTimeout):
			if time.Since(lastHeartbeat) > electionTimeout {
				log.Printf("Starting election, becoming candidate")
				state = candidate
				log.Printf("Incrementing term to %d", term+1)
				term++
				return
			}
		}
	}
}

func runCandidate() {
	log.Printf("Running as candidate...")

	votes := 1

	voteCh := make(chan bool, 2)

	for _, m := range members {
		go func(m member) {
			resp, err := http.Get(fmt.Sprintf("http://%s/request-vote?term=%d&id=%s", m.addr, term, id))
			if err != nil {
				log.Printf("Failed to request vote from %s: %v", m.addr, err)

				voteCh <- false
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				log.Printf("Failed to request vote from %s: %v", m.addr, resp.Status)

				voteCh <- false
				return
			}

			body := make([]byte, 1)
			_, err = resp.Body.Read(body)
			if err != nil && err != io.EOF {
				log.Printf("Failed to read response body: %v", err)

				voteCh <- false
				return
			}

			log.Printf("Received vote %s from %s", m.addr, string(body))

			if string(body) == "T" {
				voteCh <- true
				return
			}
		}(m)
	}

	electionTimeout := time.Duration(200+rand.Intn(300)) * time.Millisecond
	log.Printf("Election timeout: %v", electionTimeout)

	for {
		select {
		case <-time.After(electionTimeout):
			log.Printf("Election timed out")
			state = follower
			return
		case vote := <-voteCh:
			if vote {
				log.Printf("Add vote")
				votes++
				if votes > len(members)/2 {
					log.Printf("Won election with %d votes", votes)
					state = leader
					return
				}
			}
		case <-stepDown:
			state = follower
			return
		}
	}

}

func runLeader() {
	log.Printf("Running as leader...")

	go func() {
		for {
			select {
			case c := <-proposal:
				log.Printf("Received proposal %v", c)
				cLog = append(cLog, c)
				votes := 1
				voteCh := make(chan bool, 2)
				for _, m := range members {
					go func(m member) {
						url := fmt.Sprintf("http://%s/append-entries?term=%d&id=%s&commitIdx=%d&key=%s&val=%s", m.addr, term, id, commitIdx, c.key, c.val)

						log.Printf("Replicating to %v, url= %s \n", m, url)

						_, err := http.Get(url)
						if err != nil {
							log.Printf("Failed to replicate to %s: %v", m.addr, err)
							return
						}
						log.Printf("Replicated to %s", m.addr)
						voteCh <- true
					}(m)
				}

			LOOP:
				for {
					select {
					case vote := <-voteCh:
						if vote {
							votes++
							if votes > len(members)/2 {
								commitIdx++
								KVs[c.key] = c.val
								c.rsp <- nil
								break LOOP
							}
						}
					}
				}
			}
		}
	}()

	for {
		for _, m := range members {
			go func(m member) {
				resp, err := http.Get(fmt.Sprintf("http://%s/append-entries?term=%d&id=%s&commitIdx=%d", m.addr, term, id, commitIdx))
				if err != nil {
					log.Printf("Failed to send heartbeat to %s: %v", m.addr, err)
					return
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					log.Printf("Failed to send heartbeat to %s: %v", m.addr, resp.Status)
					return
				}

				//log.Printf("Sent heartbeat to %s", m.addr)
			}(m)
		}

		heartBeatInterval := time.Duration(100+rand.Intn(50)) * time.Millisecond
		time.Sleep(heartBeatInterval)
	}
}
