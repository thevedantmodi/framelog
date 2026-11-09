.PHONY: build test sim clean reset

build:
	go build -o framelogd ./cmd/framelogd

test:
	go test ./... -race

# Run the real pipeline end to end against a fake α7C II card in a temp
# sandbox — no SD card, reader, or photos needed. Pass flags with ARGS,
# e.g. make sim ARGS="-v -only backup-full".
sim:
	go run ./cmd/framelog-sim $(ARGS)

clean: reset
	rm -f framelogd

reset:
	rm -rf ~/Photos/inbox ~/Photos/originals ~/Photos/processed
	rm -f ~/Photos/catalog.db ~/Photos/framelog.log \
	      ~/Photos/.ingest_trigger ~/Photos/.outgest_trigger
