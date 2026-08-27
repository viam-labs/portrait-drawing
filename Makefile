BINARY := portrait-drawing
BIN_DIR := bin

.PHONY: setup system-deps venv models web build lint check-lint test python-test module.tar.gz clean

default: module.tar.gz

setup: system-deps venv
	go mod tidy

models:
	bash ./fetch_models.sh

web:
	cd web && npm ci && npm run build

system-deps:
	bash ./setup.sh

venv:
	bash ./setup_python.sh

build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(BINARY) ./cmd/module
	@echo "Binary: $(BIN_DIR)/$(BINARY)"

lint:
	golangci-lint run --fix ./...

check-lint:
	golangci-lint run ./...

test:
	go test ./...
	$(MAKE) python-test

python-test:
	.venv/bin/pytest python/ -v

module.tar.gz: build web
	tar czf module.tar.gz $(BIN_DIR)/$(BINARY) meta.json first_run.sh fetch_models.sh setup.sh setup_python.sh python requirements.txt web/dist
	@echo "Created module.tar.gz"

clean:
	rm -rf $(BIN_DIR) module.tar.gz web/dist
	@echo "Clean complete"
