#!/usr/bin/env bash
# systemd supplies non-secret node settings and a separate role EnvironmentFile.
set -euo pipefail
bin=${RELAIS_BIN_DIR:-/opt/relais/bin}
case ${1:-} in
  relay|relay-standby)
    unset RELAIS_SESSIONSTORE_KEY
    standby=()
    [[ $1 == relay ]] || standby=(-standby)
    exec "$bin/relais-relay" -media "$RELAY_PRIVATE:${RELAY_MEDIA_PORT:-34780}" -leg "$RELAY_PRIVATE:${RELAY_LEG_PORT:-34781}" \
      -http "$RELAY_PRIVATE:${RELAY_HTTP_PORT:-34700}" -relay-lease preview "${standby[@]}"
    ;;
  worker)
    exec "$bin/relais-worker" -name "$WORKER_NAME" -media "$NODE_PRIVATE:${WORKER_MEDIA_PORT:-34711}" \
      -http "$NODE_PRIVATE:${WORKER_HTTP_PORT:-34701}" -relay-leg "$RELAY_PRIVATE:${RELAY_LEG_PORT:-34781}" \
      -relay-media "$RELAY_PUBLIC:${RELAY_MEDIA_PORT:-34780}" -control "http://$CONTROL_PRIVATE:${CONTROL_HTTP_PORT:-34710}" -agent echo
    ;;
  control)
    workers=(-worker "w1=http://$WORKER_ONE:${WORKER_ONE_PORT:-34701}" -worker "w2=http://$WORKER_TWO:${WORKER_TWO_PORT:-34701}")
    [[ -z ${WORKER_THREE:-} ]] || workers+=(-worker "w3=http://$WORKER_THREE:${WORKER_THREE_PORT:-34701}")
    exec "$bin/relais-control" -http "$CONTROL_PRIVATE:${CONTROL_HTTP_PORT:-34710}" -relay "http://$RELAY_PRIVATE:${RELAY_HTTP_PORT:-34700}" \
      -worker-call-cap "$WORKER_CALL_CAP" "${workers[@]}"
    ;;
  signal)
    exec "$bin/relais-signal" -listen "$NODE_PRIVATE:${SIGNAL_PORT:-34743}" \
      -control "http://$CONTROL_PRIVATE:${CONTROL_HTTP_PORT:-34710}" -advertise-ips "$NODE_PRIVATE" \
      -token-file "${CREDENTIALS_DIRECTORY:?}/signal.token"
    ;;
  *) echo 'unknown component; start refused' >&2; exit 2 ;;
esac
