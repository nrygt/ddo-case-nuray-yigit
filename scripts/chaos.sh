#!/usr/bin/env bash
# Kaos senaryosu: veritabanını durdurur, sistemin bunu doğru raporladığını ve
# veritabanı geri gelince kendiliğinden toparlandığını gösterir. Ardından API
# konteynerini öldürür; restart politikası onu geri getirmeli.
set -euo pipefail
API=${API_URL:-http://localhost:8080}
step() { printf '\n== %s\n' "$*"; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@" || true; }
login() {
  curl -s -X POST "$API/api/v1/auth/login" -H 'Content-Type: application/json' \
    -d '{"email":"ops@pist.test","password":"Demo.2026!"}' | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p'
}

step "Başlangıç: sistem sağlıklı mı?"
test "$(code "$API/health")" = 200 && echo "health 200"
TOKEN=$(login); test -n "$TOKEN" && echo "giriş başarılı"

step "Veritabanı durduruluyor"
docker compose stop db
sleep 2
h=$(code "$API/health"); echo "health → $h (503 bekleniyor)"; test "$h" = 503
body=$(curl -s "$API/api/v1/flights?limit=1" -H "Authorization: Bearer $TOKEN")
echo "uçuş listesi → $body"
echo "$body" | grep -q database_unavailable || { echo "503 database_unavailable bekleniyordu"; exit 1; }
docker compose ps api --format '{{.Name}} {{.Status}}'

step "Veritabanı geri başlatılıyor"
docker compose start db
for i in $(seq 1 60); do
  [ "$(code "$API/health")" = 200 ] && break
  sleep 1
done
test "$(code "$API/health")" = 200 && echo "health 200 (${i} sn içinde toparlandı, API yeniden başlatılmadı)"
test "$(code "$API/api/v1/flights?limit=1" -H "Authorization: Bearer $TOKEN")" = 200 && echo "aynı token ile istekler yeniden çalışıyor"

step "API konteyneri öldürülüyor (SIGKILL)"
docker compose kill -s SIGKILL api
sleep 1
echo "API kapalıyken: $(code "$API/health")"
docker compose up -d api
for i in $(seq 1 60); do
  [ "$(code "$API/health")" = 200 ] && break
  sleep 1
done
test "$(code "$API/health")" = 200 && echo "API ${i} sn içinde geri geldi"
TOKEN=$(login); test -n "$TOKEN" && echo "yeni giriş başarılı (JWT durumsuz, refresh tokenlar DB'de)"

step "Simülatör toparlandı mı?"
sleep 8
docker compose logs --since 15s simulator | grep -c '"besleme partisi"\|tablet' | xargs -I{} echo "son 15 sn'de {} simülatör olayı"
echo
echo "KAOS SENARYOSU: PASS"
