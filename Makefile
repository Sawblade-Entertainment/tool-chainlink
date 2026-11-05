BINARY := chainlink
SRC    := src
BIN    := bin
LDFLAGS := -s -w

# os-arch pairs built by `make build`
PLATFORMS := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64 windows-arm64

TARGETS := $(foreach p,$(PLATFORMS),$(BIN)/$(BINARY)-$(p)$(if $(findstring windows,$(p)),.exe))

.PHONY: all build local test clean windows macos linux $(PLATFORMS)

# Run the tests once, then build every platform.
all: test build

build: $(TARGETS)

windows: windows-amd64 windows-arm64
macos: darwin-amd64 darwin-arm64
linux: linux-amd64 linux-arm64

# Each platform target runs the tests first.
$(PLATFORMS): %: test
	@$(MAKE) --no-print-directory $(BIN)/$(BINARY)-$*$(if $(findstring windows,$*),.exe)

$(BIN)/$(BINARY)-%:
	@mkdir -p $(BIN)
	cd $(SRC) && GOOS=$(word 1,$(subst -, ,$(basename $*))) GOARCH=$(word 2,$(subst -, ,$(basename $*))) \
		CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o ../$@ .

# Build for the current platform and install it to ~/.chainlink/bin.
LOCAL_DIR := $(HOME)/.chainlink/bin
GOHOSTOS  := $(shell go env GOHOSTOS)
EXE       := $(if $(filter windows,$(GOHOSTOS)),.exe)

local: test
	@mkdir -p $(BIN) $(LOCAL_DIR)
	cd $(SRC) && CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o ../$(BIN)/$(BINARY)$(EXE) .
	cp $(BIN)/$(BINARY)$(EXE) $(LOCAL_DIR)/$(BINARY)$(EXE)
	@echo "Installed $(LOCAL_DIR)/$(BINARY)$(EXE)"

test:
	cd $(SRC) && go test ./...

clean:
	rm -rf $(BIN)
