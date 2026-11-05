package agenttest

// The script's variable data is shell-quoted at construction. Only the
// runner's own JSON-RPC requests are inspected at runtime; result streams are
// marshaled by Go, keeping arbitrary scripted text out of shell evaluation.
const sessionPrelude = `
record=''
invocation=1
while :; do
  candidate="$root/invocation-$invocation"
  if /bin/mkdir "$candidate" 2>/dev/null; then
    record=$candidate
    break
  fi
  [ -d "$candidate" ] || break
  invocation=$((invocation + 1))
done
if [ -n "$record" ]; then
  { printf '%s\000' "$@" > "$record/args"
    /usr/bin/env -0 > "$record/env"
    printf '%s' "$PWD" > "$record/cwd"
  } 2>/dev/null
fi
session_dir=''
origins=''
separator=''
previous=''
for argument in "$@"; do
  if [ "$previous" = '--mcp-config' ]; then session_dir=${argument%/*}; fi
  if [ "$previous" = '-c' ]; then
    key=${argument%%=*}
    case "$key" in
      shell_environment_policy.set.*SESSION_DIR)
        session_dir=${argument#*=}
        session_dir=${session_dir#\"}
        session_dir=${session_dir%\"}
        ;;
    esac
    # Only simple dotted keys are settings. MCP table keys may contain quoted
    # names and are not settings the held configuration check asks about.
    case "$key" in
      ''|*[!a-zA-Z0-9_.]*) ;;
      *) origins="$origins$separator\"$key\":{\"name\":{\"type\":\"sessionFlags\"}}"; separator=',' ;;
    esac
  fi
  previous=$argument
done
claim_turn() {
  [ "${claimed:-}" != yes ] || return
  claimed=yes
  turn=1
  # Each directory is an atomic, durable claim. No lock can be left behind
  # when a timed-out process is killed between claiming and recording.
  while ! /bin/mkdir "$root/start-$turn" 2>/dev/null; do
    [ -d "$root/start-$turn" ] || return
    turn=$((turn + 1))
  done
  if [ -n "$record" ]; then printf '%s\n' "$turn" > "$record/session" 2>/dev/null; fi
}
wait_forever() { while :; do /bin/sleep 60; done; }
unexpected() {
  echo "agenttest: unrecognized $backend invocation or request: $*" >&2
  exit 64
}
`

const sessionDriver = `
case "$backend:$1:$2" in
  codex:mcp:list) printf '[]\n'; exit 0 ;;
  claude:mcp:list|claude:mcp:) printf 'No MCP servers configured.\n'; exit 0 ;;
  codex:features:list) exit 0 ;;
esac
if [ "$backend" = claude ]; then
  [ "$1" = '-p' ] || unexpected "$@"
  claim_turn
  load_turn
  # stdin is a bounded prompt, unlike app-server's ongoing conversation.
  if [ -n "$record" ]; then
    { while IFS= read -r line; do
        printf '%s\n' "$line"
      done
      printf '%s' "$line"
    } > "$record/prompt"
  else
    while IFS= read -r line; do :; done
  fi
  eval "$actions"
  [ "$ending" != never-ends ] || wait_forever
  if [ "$ending" = rate-limited ]; then printf '%s\n' "$rate_event"; fi
  printf '%s\n' "$claude_result"
  exit "$exit_code"
fi
[ "$1" = app-server ] || unexpected "$@"
while IFS= read -r line; do
  if [ -n "$record" ]; then printf '%s\n' "$line" >> "$record/requests" 2>/dev/null; fi
  # Requests are compact JSON emitted by encoding/json. Notifications have no
  # id and do not need an answer. IDs belong to the client, not to the script.
  id=${line#*'"id":'}
  id=${id%%,*}
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    *'"method":"initialized"'*) ;;
    *'"method":"config/read"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"config":{},"origins":{%s}}}\n' "$id" "$origins"
      ;;
    *'"method":"thread/start"'*|*'"method":"thread/resume"'*)
      claim_turn
      load_turn
      if [ "$ending" = thread-error ]; then
        eval "$actions"
        printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32600,"message":%s}}\n' "$id" "$message_json"
      else
        printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":%s}}}\n' "$id" "$thread_json"
      fi
      ;;
    *'"method":"turn/start"'*)
      [ "${claimed:-}" = yes ] || unexpected "$line"
      # Refuse a bare prompt string, which the app-server protocol rejects.
      case "$line" in *'"input":[{'*'"type":"text"'*) ;; *) unexpected "$line" ;; esac
      printf '{"jsonrpc":"2.0","id":%s,"result":{"turn":{"id":"scripted-turn"}}}\n' "$id"
      eval "$actions"
      [ "$ending" != never-ends ] || wait_forever
      printf '%s' "$events"
      ;;
    *'"method":"turn/interrupt"'*) ;;
    *) unexpected "$line" ;;
  esac
done
`
