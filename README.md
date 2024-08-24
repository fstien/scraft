### Scraft - Raft from Scratch

Learning the raft consensus protocol. 

### Running locally

```bash
go run main.go -id="node0" -addr="localhost:8080" -members="node1;localhost:8081,node2;localhost:8082"
go run main.go -id="node1" -addr="localhost:8081" -members="node0;localhost:8080,node2;localhost:8082"
go run main.go -id="node2" -addr="localhost:8082" -members="node0;localhost:8080,node1;localhost:8081"
```

Set a KV: 

```bash 
curl -v "http://localhost:8080/set?key=yourKey&val=yourKey"
```

Read it back:

```bash
curl -v "http://localhost:8081/get?key=yourKey"
```