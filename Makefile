# binlogsum — build helpers
BINARY := binlogsum
VERSION := 0.1.0

.PHONY: build test run clean install snapshot

build:
	go build -o $(BINARY) .

test:
	go test ./...

install:
	go install .

# Render the bundled test fixture as a snapshot you can open in a browser.
snapshot: build
	./$(BINARY) --file testdata/sample.binlog --mode snapshot --out example.html

clean:
	rm -f $(BINARY) example.html
