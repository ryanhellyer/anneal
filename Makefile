BINARY := anneal

.PHONY: build test vet clean

build:
	CGO_ENABLED=0 go build -trimpath -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
