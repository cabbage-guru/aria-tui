.PHONY: build run clean install check

BINARY=aria-tui

build:
	go build -o $(BINARY) ./cmd/aria-tui/

run: build
	./$(BINARY)

install: build
	cp $(BINARY) $(GOPATH)/bin/ 2>/dev/null || cp $(BINARY) /usr/local/bin/

clean:
	rm -f $(BINARY)

check:
	./$(BINARY) check

# Import all .conf files from a directory
import-vpn:
	@if [ -z "$(DIR)" ]; then echo "Usage: make import-vpn DIR=/path/to/configs"; exit 1; fi
	./$(BINARY) import-dir $(DIR)
