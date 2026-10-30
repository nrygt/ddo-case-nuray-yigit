#!/usr/bin/env bash
# API ve arayüz sağlıklı olana kadar bekler (en fazla 5 dk).
set -euo pipefail
deadline=$((SECONDS + 300))
for url in http://localhost:8080/health http://localhost:3000/; do
  until curl -fsS -o /dev/null "$url"; do
    if (( SECONDS > deadline )); then echo "zaman aşımı: $url"; docker compose ps; exit 1; fi
    sleep 2
  done
  echo "hazır: $url"
done
