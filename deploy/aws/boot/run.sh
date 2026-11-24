#!/usr/bin/env bash
# systemd supplies non-secret node settings and a separate role EnvironmentFile.
set -euo pipefail
bin=${RELAIS_BIN_DIR:-/opt/relais/bin}
case ${1:-} in
  relay|relay-standby)
    unset RELAIS_SESSIONSTORE_KEY
    args=(-media "$RELAY_PRIVATE:${RELAY_MEDIA_PORT:-24780}" -leg "$RELAY_PRIVATE:${RELAY_LEG_PORT:-24781}"
      -http "$RELAY_PRIVATE:${RELAY_HTTP_PORT:-24700}" -relay-lease "${RELAY_SLOT:-preview}")
    [[ $1 == relay ]] || args+=(-standby)
    if [[ -n ${RELAY_TWO_PRIVATE:-} ]]; then
      args+=(-relay-slot "${RELAY_SLOT:-preview}" -advertise-media "$RELAY_PUBLIC:${RELAY_MEDIA_PORT:-24780}")
    fi
    exec "$bin/relais-relay" "${args[@]}"
    ;;
  worker)
    exec "$bin/relais-worker" -name "$WORKER_NAME" -media "$NODE_PRIVATE:${WORKER_MEDIA_PORT:-24711}" \
      -http "$NODE_PRIVATE:${WORKER_HTTP_PORT:-24701}" -relay-leg "$RELAY_PRIVATE:${RELAY_LEG_PORT:-24781}" \
      -relay-media "$RELAY_PUBLIC:${RELAY_MEDIA_PORT:-24780}" -control "http://$CONTROL_PRIVATE:${CONTROL_HTTP_PORT:-24710}" -agent echo
    ;;
  control)
    workers=(-worker "w1=http://$WORKER_ONE:${WORKER_ONE_PORT:-24701}" -worker "w2=http://$WORKER_TWO:${WORKER_TWO_PORT:-24701}")
    [[ -z ${WORKER_THREE:-} ]] || workers+=(-worker "w3=http://$WORKER_THREE:${WORKER_THREE_PORT:-24701}")
    relays=(-relay "http://$RELAY_PRIVATE:${RELAY_HTTP_PORT:-24700}")
    if [[ -n ${RELAY_TWO_PRIVATE:-} ]]; then
      relays=(-relay "preview=http://$RELAY_PRIVATE:${RELAY_HTTP_PORT:-24700}"
        -relay "preview-two=http://$RELAY_TWO_PRIVATE:${RELAY_TWO_HTTP_PORT:-24700}")
    fi
    exec "$bin/relais-control" -http "$CONTROL_PRIVATE:${CONTROL_HTTP_PORT:-24710}" "${relays[@]}" \
      -worker-call-cap "$WORKER_CALL_CAP" "${workers[@]}"
    ;;
  signal)
    exec "$bin/relais-signal" -listen "$NODE_PRIVATE:${SIGNAL_PORT:-24743}" \
      -control "http://$CONTROL_PRIVATE:${CONTROL_HTTP_PORT:-24710}" -advertise-ips "$NODE_PRIVATE" \
      -token-file "${CREDENTIALS_DIRECTORY:?}/signal.token"
    ;;
  *) echo 'unknown component; start refused' >&2; exit 78 ;;
esac
