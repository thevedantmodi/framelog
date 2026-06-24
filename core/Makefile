.PHONY: build test clean reset

build:
	go build -o framelogd ./cmd/framelogd

test:
	go test ./... -race

clean: reset
	rm -f framelogd

reset:
	rm -rf ~/Photos/inbox ~/Photos/originals ~/Photos/processed
	rm -f ~/Photos/catalog.db ~/Photos/framelog.log \
	      ~/Photos/.ingest_trigger ~/Photos/.outgest_trigger
