#!/usr/bin/env bash
set -Eeuo pipefail

BOLD='\033[1m'; DIM='\033[2m'; GREEN='\033[32m'; CYAN='\033[36m'; YELLOW='\033[33m'; RED='\033[31m'; RESET='\033[0m'
ok()   { printf "✅ %s\n" "$*"; }
info() { printf "ℹ️  %s\n" "$*"; }
warn() { printf "⚠️  %s\n" "$*"; }
err()  { printf "❌ %b%s%b\n" "$RED" "$*" "$RESET"; }
trap 'err "Failed on line $LINENO: $BASH_COMMAND"' ERR

banner_bidhan() {
  printf "\033[1;36m"
  cat <<'ASCII'
 ____  _ _ _        _            ____              _ _              
| __ )(_) (_)___   | |__  _   _ | __ )  __ _  __ _(_) |_ ___ _ __  
|  _ \| | | / __|  | '_ \| | | ||  _ \ / _` |/ _` | | __/ _ \ '__| 
| |_) | | | \__ \  | |_) | |_| || |_) | (_| | (_| | | ||  __/ |    
|____/|_|_|_|___/  |_.__/ \__, ||____/ \__,_|\__, |_|\__\___|_|    
                          |___/               |___/                
            B I D H A N   B A N I Y A
ASCII
  printf "\033[0m\n🚀 Go Build Helper — loads .env, outputs to ./bin/\n\n"
}

ENV_FILE=".env"
OUT_NAME="app"
SRC_DIR="."
OUT_DIR="./bin"
GOOS_VAL=""
GOARCH_VAL=""
TAGS=""
LDFLAGS=""
RACE="false"
TIDY="false"
CLEAN_CACHE="false"

usage() {
cat <<EOF
${BOLD}Usage:${RESET} $0 [options]

Options:
  --env FILE         .env file to load (default: ./.env)
  --out NAME         output binary name (default: app)
  --dir PATH         source dir/package (default: .)
  --os GOOS          target OS (e.g. linux, darwin, windows)
  --arch GOARCH      target ARCH (e.g. amd64, arm64)
  --tags "t1 t2"     space-separated build tags
  --ldflags "..."    extra ldflags (quoted)
  --race             enable race detector (CGO_ENABLED=1)
  --tidy             run 'go mod tidy' before build
  --clean-cache      run 'go clean -cache -testcache -modcache' first
  -h, --help         show help

Examples:
  $0 --env .env --out nlq-server
  $0 --env .env --out nlq-server --os linux --arch amd64
  $0 --tags "prod sqlite" --ldflags "-s -w" --tidy
  $0 --clean-cache --race
EOF
}

# -------- arg parse --------
while [ $# -gt 0 ]; do
  case "$1" in
    --env) ENV_FILE="$2"; shift 2 ;;
    --out) OUT_NAME="$2"; shift 2 ;;
    --dir) SRC_DIR="$2"; shift 2 ;;
    --os) GOOS_VAL="$2"; shift 2 ;;
    --arch) GOARCH_VAL="$2"; shift 2 ;;
    --tags) TAGS="$2"; shift 2 ;;
    --ldflags) LDFLAGS="$2"; shift 2 ;;
    --race) RACE="true"; shift ;;
    --tidy) TIDY="true"; shift ;;
    --clean-cache) CLEAN_CACHE="true"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) err "Unknown arg: $1"; usage; exit 1 ;;
  esac
done

banner_bidhan

# -------- helpers --------
require_cmd() { command -v "$1" >/dev/null 2>&1 || { err "Missing \`$1\`"; exit 1; }; }
require_cmd go

if [ ! -d "$SRC_DIR" ] && [[ "$SRC_DIR" != .* && "$SRC_DIR" != github.com/* ]]; then
  warn "SRC_DIR '$SRC_DIR' is not a local dir; assuming package path"
fi

if [ -f "$ENV_FILE" ]; then
  info "Loading env from ${BOLD}$ENV_FILE${RESET}"
  set -a; . "$ENV_FILE"; set +a
else
  warn "No $ENV_FILE found; continuing with current env"
fi

if [ "${CLEAN_CACHE}" = "true" ]; then
  info "Clearing Go caches…"
  go clean -cache -testcache -modcache || true
fi

if [ "${TIDY}" = "true" ]; then
  info "Running ${BOLD}go mod tidy${RESET}…"
  (cd "${SRC_DIR}" && go mod tidy)
fi

mkdir -p "$OUT_DIR"

BUILD_ENV=()
[ -n "$GOOS_VAL" ] && BUILD_ENV+=(GOOS="$GOOS_VAL")
[ -n "$GOARCH_VAL" ] && BUILD_ENV+=(GOARCH="$GOARCH_VAL")
if [ "$RACE" = "true" ]; then
  export CGO_ENABLED=1
else
  : "${CGO_ENABLED:=0}"; export CGO_ENABLED
fi

ARGS=( build -o "${OUT_DIR}/${OUT_NAME}" )
[ -n "$TAGS" ] && ARGS+=( -tags "$TAGS" )
[ -n "$LDFLAGS" ] && ARGS+=( -ldflags "$LDFLAGS" )
[ "$RACE" = "true" ] && ARGS+=( -race )
ARGS+=( "$SRC_DIR" )

info "go version: $(go version)"
[ -n "$GOOS_VAL" ] && info "Target OS: ${GOOS_VAL}"
[ -n "$GOARCH_VAL" ] && info "Target Arch: ${GOARCH_VAL}"
info "Output: ${BOLD}${OUT_DIR}/${OUT_NAME}${RESET}"
[ -n "$TAGS" ] && info "Tags: $TAGS"
[ -n "$LDFLAGS" ] && info "Ldflags: $LDFLAGS"
[ "$RACE" = "true" ] && info "Race: enabled"

info "Building…"
"${BUILD_ENV[@]}" go "${ARGS[@]}"

ok "Build complete → ${BOLD}${OUT_DIR}/${OUT_NAME}${RESET}"
printf "%b" "${DIM}Tip:${RESET} load env and run:\n"
echo "  set -a; source ${ENV_FILE}; set +a"
echo "  ${OUT_DIR}/${OUT_NAME}"
