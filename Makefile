APP := operetta-server
PKG := .
DIST := dist
LDFLAGS := -s -w
BUILD_FLAGS := -trimpath -ldflags "$(LDFLAGS)"
PLATFORMS := windows/amd64 linux/amd64 windows/arm64 linux/arm64

.PHONY: build build-all clean

build:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 go build $(BUILD_FLAGS) -o $(DIST)/$(APP) $(PKG)
	@echo Built $(DIST)/$(APP)

build-all:
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
	  os=$${p%%/*}; arch=$${p##*/}; \
	  ext=""; if [ "$$os" = windows ]; then ext=".exe"; fi; \
	  echo Building $(DIST)/$(APP)-$$os-$$arch$$ext ; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(BUILD_FLAGS) -o $(DIST)/$(APP)-$$os-$$arch$$ext $(PKG); \
	done
	@echo Binaries are in ./$(DIST)

clean:
	rm -rf $(DIST)

