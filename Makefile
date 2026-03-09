.PHONY: build build-image run clean install

BINARY=aria-tui
DOCKER_IMAGE=aria-tui-vpn:latest

build:
	go build -o $(BINARY) ./cmd/aria-tui/

build-image:
	docker build -t $(DOCKER_IMAGE) docker/

run: build
	./$(BINARY)

install: build
	cp $(BINARY) $(GOPATH)/bin/ 2>/dev/null || cp $(BINARY) /usr/local/bin/

clean:
	rm -f $(BINARY)

# Import all .conf files from a directory
import-vpn:
	@if [ -z "$(DIR)" ]; then echo "Usage: make import-vpn DIR=/path/to/configs"; exit 1; fi
	./$(BINARY) import-dir $(DIR)
