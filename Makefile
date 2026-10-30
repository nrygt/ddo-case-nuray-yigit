# PistKoordinat — sık kullanılan komutlar. Hepsi repo kökünden çalışır.
COMPOSE ?= docker compose

.PHONY: help up down logs verify reset-demo load load-k6 chaos test test-unit test-db cover lint

help:
	@echo "make up          sistemi ayağa kaldır (http://localhost:3000)"
	@echo "make verify      tüm özellik ve görevleri uçtan uca dene (PASS/FAIL tablosu)"
	@echo "make reset-demo  veriyi ilk seed haline döndür"
	@echo "make load        60 sn karma yük testi (p50/p95/p99)"
	@echo "make chaos       veritabanını durdur, toparlanmayı göster"
	@echo "make test        Go testleri (Postgres gerekir: make test-db)"
	@echo "make down        durdur (veriyi korur); 'make down V=1' veriyi de siler"

.env:
	cp .env.example .env

up: .env
	$(COMPOSE) up --build -d
	./scripts/wait-healthy.sh
	@echo "Arayüz: http://localhost:3000   API: http://localhost:8080"

down:
	$(COMPOSE) --profile verify --profile load down $(if $(V),-v,)

logs:
	$(COMPOSE) logs -f --tail=100 api simulator

# verify servisi api'nin sağlıklı olmasını bekler (depends_on: service_healthy)
verify: .env
	$(COMPOSE) up -d
	$(COMPOSE) --profile verify run --rm verify

reset-demo: .env
	$(COMPOSE) stop simulator
	$(COMPOSE) run --rm --entrypoint /app/migrate migrate -reset
	$(COMPOSE) start simulator

load: .env
	$(COMPOSE) --profile load run --rm loadtest

# k6 kuruluysa aynı senaryonun k6 sürümü
load-k6:
	k6 run loadtest/k6.js

chaos:
	./scripts/chaos.sh

# Testler: alan paketleri her yerde koşar; entegrasyon testleri TEST_DATABASE_URL ister.
test-unit:
	cd backend && go test -race -count=1 ./...

test-db:
	docker run -d --rm --name pk-test-db -e POSTGRES_PASSWORD=test -e POSTGRES_DB=pist_test -p 55432:5432 postgres:16-alpine
	@until docker exec pk-test-db pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done

test:
	cd backend && TEST_DATABASE_URL=$${TEST_DATABASE_URL:-postgres://postgres:test@localhost:55432/pist_test?sslmode=disable} \
		go test -race -count=1 -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

cover:
	cd backend && ./scripts/coverage.sh coverage.out

lint:
	cd backend && go vet ./... && gofmt -l . | (! grep .)
	node --check frontend/public/app.js
