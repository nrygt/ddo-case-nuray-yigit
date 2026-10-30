# Test ve doğrulama rehberi

## Hızlı yol

```bash
cp .env.example .env
docker compose up --build -d     # ilk açılış: migrasyon + seed otomatik
make verify                      # tüm kimlikler uçtan uca, sonda PASS/FAIL tablosu
make reset-demo                  # veriyi ilk seed haline döndür (verify'ı tekrarlamak için)
```

`make verify` ≈ 3 dakika sürer. En uzun iki adım bilerek uzun: T-4 2000 olayı 60 saniyeye yayarak akıtır, T-5 ilk çalıştırmada 5 milyon görev olayını üretir (~90 sn). Herhangi bir kontrol başarısız olursa çıkış kodu 1'dir. Uzun yük testi ayrı: `make load`.

Örnek çıktı (yerel çalıştırma, temiz seed):

```
KİMLİK  SONUÇ  SÜRE    AÇIKLAMA
HEALTH  PASS   0.0s    GET /health + veritabanı
AUTH-1  PASS   0.2s    Yanlış parola 401
AUTH-2  PASS   0.0s    Token yok / sahte token 401
AUTH-3  PASS   0.1s    Refresh rotasyonu, tekrar kullanım reddi
RBAC-1  PASS   0.0s    Yetkisiz rol 403 (her rol için örnek)
RBAC-2  PASS   0.0s    Havayolu temsilcisi yalnızca kendi verisi
F-1     PASS   0.0s    Uçuş çizelgesi, keyset sayfalama, oluşturma
F-2     PASS   0.1s    Stand/kapı envanteri, tip uyumu
F-3     PASS   0.0s    Stand/kapı tahsisi, elle atama çakışması
T-1     PASS   0.1s    Stand tahsis çizelgeleme (kurallar, süre, rapor)
F-4     PASS   0.0s    Ekipler, ekipman, görevler
T-3     PASS   0.2s    Ekip atama (belge, dinlenme, ekipman, risk)
F-5     PASS   0.0s    Gecikme/değişiklik olayları (besleme)
T-2     PASS   0.1s    Gecikme yayılımı, what-if, uygulama
F-6     PASS   0.3s    Canlı pano + SSE
T-4     PASS   60.2s   Çevrimdışı senkronizasyon + 2000 olay/dk akış
F-7     PASS   0.0s    Turnaround performans raporu
T-5     PASS   110.6s  5M olayda analitik < 2 sn, EXPLAIN, veri kalitesi
OPS-1   PASS   0.0s    Denetim kaydı, metrikler, OpenAPI

19/19 PASS, toplam 172 sn
```

## (a) Demo hesapları

Hepsinin parolası **`Demo.2026!`**

| Rol | E-posta | Ne görür |
|---|---|---|
| Operasyon kontrolörü | `ops@pist.test` | Pano, uçuşlar, standlar (tahsis çalıştırma, stand kapatma), gecikme planlama (uygulama yetkisi), raporlar |
| Yer hizmetleri şefi | `sef@pist.test` | Pano, ekipler ve görevler (atama çalıştırma, vardiya ekleme), gecikme önizleme, uçuşlar, standlar |
| Ekip lideri (mobil) | `ekip@pist.test` | Yalnızca kendi ekibinin (Yakıt-1) görevleri, çevrimdışı başladı/bitti |
| Havayolu temsilcisi | `havayolu@pist.test` | Yalnızca YK havayolunun uçuşları ve stand çizelgesi (salt okunur) |
| Yönetim | `yonetim@pist.test` | Pano, raporlar, veri kalitesi, denetim kaydı |

Ek ekip liderleri: `ekip2@pist.test` (Bagaj-1), `ekip3@pist.test` (Temizlik-1).

Seed'deki bilinçli istisnalar: **Yakıt-10**'un yakıt ikmal sertifikası dün dolmuş; **Bagaj-14**'ün bugünkü vardiyası dünkü vardiyadan 8 saat sonra başlıyor (11 saat kuralı ihlali). Atama bu ikisini kullanmamalı.

## (b) Kimlik bazında nasıl denenir / beklenen sonuç

| Kimlik | Nasıl denenir | Beklenen sonuç |
|---|---|---|
| HEALTH | `curl localhost:8080/health` | `200`, `"database":"ok"`. Veritabanı durursa `503` |
| AUTH | `requests.http` → "Yanlış parola"; token'sız `GET /api/v1/flights` | `401 invalid_credentials` (var olmayan e-posta için de aynı yanıt), `401 unauthorized` |
| AUTH (refresh) | Aynı refresh token'ı iki kez `/auth/refresh`'e gönder | İlki yeni çift döner, ikincisi `401 token_reused` ve kullanıcının tüm oturumları kapanır |
| RBAC | Ekip lideri `GET /flights`, havayolu `POST /allocations/run`, şef `POST /replan/apply` | `403 forbidden`. Havayolu başka havayolunun uçuş detayına `404` alır |
| F-1 | `GET /api/v1/flights?limit=37` ve `next_cursor` ile sayfalama; `POST /flights` iki kez aynı `Idempotency-Key` ile | Tüm gün tekrarsız dolaşılır; ikinci istek aynı gövdeyi `Idempotent-Replayed: true` ile döner; aynı anahtar farklı gövdeyle `422` |
| F-2 | `GET /api/v1/stands?type=B77W` | Yalnızca E sınıfı standlar (A5, A6, R9) |
| F-3 | `GET /allocations`; dolu bir standa çakışan elle atama `POST /allocations` | Liste; çakışmada `409 conflict` (veritabanı `EXCLUDE`) |
| F-4 | `GET /crews`, `/equipment`, `/tasks`; ekip lideriyle `/my/tasks` | 41 ekip, 4 yakıt aracı; ekip lideri yalnızca Yakıt-1 görevlerini görür |
| F-5 | `POST /feed/events` ile aynı uçuşa seq 3, 1, 2, 3 | Özet `APPLIED 1, STALE 2, DUPLICATE 1`; son durum seq 3'ün saati, `DELAYED` |
| F-6 | Arayüzde Pano; `POST /notices` | Sayı şeridi, plandan sapanlar, riskler; duyuru "Son olaylar"a anında düşer |
| F-7 | Arayüzde Raporlar ya da `GET /reports/turnaround?group_by=airline` | Grup başına adet, medyan, P90, hedef, ihlal oranı |
| T-1 | `POST /allocations/run` | Süre ms düzeyinde; çözülemeyen her turnaround nedeni ve engelleyenleriyle; istemci tarafı kontrol: 15 dk tampon ve boyut uyumu ihlali yok |
| T-2 | Bir gelişe besleme ile +85 dk → `GET /replan/pending` → `POST /replan/preview` → `POST /replan/apply` (parmak iziyle) | Önizleme hiçbir şey yazmaz; uygulama sonrası kurallar korunur; eski parmak izi `409 plan_changed`; aynı anahtarla tekrar idempotent |
| T-3 | `POST /assignments/run` (şef) | Ekip çakışması yok, aynı anda en fazla 4 yakıt görevi, Yakıt-10 ve Bagaj-14 sabah kullanılmıyor, riskler `WARNING/CRITICAL`; 11 saatten az dinlenmeli vardiya eklemek `422 rest_rule_violation` |
| T-4 | Ekip lideri `FINISHED`+`STARTED`'ı ters sırada, şef daha erken `STARTED` gönderir; partiyi tekrar gönder | Sonuç `DONE`, başlangıç = en erken bildirilen; tekrar `DUPLICATE`; başka ekibin görevi `REJECTED`. 2000 olay/dk SSE'de kayıpsız |
| T-5 | `make verify` (5M olay üretir) ya da `GET /reports/turnaround` | Rapor < 2 sn (ölçülen 70–200 ms), `percentile_disc` ile aynı sonuç, veri kalitesi sorunları raporlanır |

## (c) Arayüzde elle denenecek kısa senaryolar

Adres: <http://localhost:3000>. Giriş ekranındaki demo hesap bağlantıları formu doldurur.

1. **Canlı pano** — `ops` ile gir. Simülatör birkaç saniyede bir besleme partisi gönderir; "Son olaylar" akmaya, "Plandan sapan uçuşlar" dolmaya başlar.
2. **Gecikme önizleme** — Panoda sapan bir uçuşta *Önizle*'ye bas. Gecikme planlama ekranında kayan turnaroundlar (eski saat üstü çizili), stand değişiklikleri ve riskler görünür. *Planı uygula* ile uygula; aynı uçuş sapanlar listesinden düşer.
3. **Stand çizelgesi** — *Standlar*'da sarı çizgi "şimdi". *Tahsisi yeniden çöz* süreyi ve yerleşemeyenleri gösterir. Envanterden bir E standını kapatıp yeniden çözerek etkisini gör, sonra tekrar aç.
4. **Çevrimdışı ekip lideri** — `ekip` ile telefon genişliğinde (tarayıcı geliştirici araçları, 390 px) gir. Ağ sekmesinden *Offline* yap, bir göreve *Başladı* bas: "Çevrimdışı. 1 olay cihazda saklanıyor." görünür. Ağı geri aç: olay gönderilir, görev "Sürüyor" olur. Aynı anda başka sekmede `sef` ile pano açıksa olay oraya düşer.
5. **Ekip ataması** — `sef` ile *Ekipler ve görevler* → *Ekipleri yeniden ata*. Risk özeti, dinlenme kuralı ihlali (Bagaj-14) ve atanamayan görevlerin nedenleri görünür.
6. **Raporlar** — `yonetim` ile *Raporlar*: tip/havayolu/stand gruplaması; *Veri kalitesi* bölümü tutarsız zaman damgalarını sayar.
7. **Yetki** — `havayolu` ile gir: menüde yalnızca Uçuşlar ve Standlar var, liste yalnızca YK.

## (d) Zorlu görevlerin kanıt komutları

| Görev | Komut | Ne kanıtlar |
|---|---|---|
| T-1 | `cd backend && go test ./internal/domain/allocation -run . -bench Solve -v` | Tampon (14 dk ret / 15 dk kabul), boyut, köprü, tercih edilen stand, takas onarımı, 20 rastgele günde kural ihlali olmaması ve "yerleşmeyen için gerçekten boş stand yok"; 300 turnaround ~3 ms |
| T-1 (DB) | `make test` → `TestStandExcludeRace`, `TestStandBufferEnforcedByDatabase`, `TestSizeAndBridgeGuardTrigger` | 20 eşzamanlı yazımda tek kazanan; tampon ve boyut veritabanında da korunuyor |
| T-2 | `go test ./internal/domain/propagation ./internal/domain/feed -v` ve `make test` → `TestReplanApplyEndToEnd`, `TestFeedOutOfOrderConcurrentConverges` | Zincir yayılımı, en az değişiklik, determinizm; sırasız/tekrarlı/eşzamanlı beslemede aynı son durum |
| T-3 | `go test ./internal/domain/crew -v` ve `make test` → `TestEquipmentExcludeRace`, `TestShiftRestRuleOnInsert` | Sertifika, 11 saat dinlenme, 4 araç sınırı, geçiş süresi, risk uyarıları; araç çakışması DB'de |
| T-4 | `go test ./internal/domain/tasksync ./internal/stream -v` ve `make test` → `TestTwoDevicesSyncSameTask` | Katlamanın sıra bağımsızlığı; iki cihazın eşzamanlı senkronizasyonu; SSE'de 2000 olayın 5 istemciye kayıpsız ulaşması |
| T-5 | `make test` → `TestAnalyticsReportAndDataQuality`, `TestPartitionPruning`; `make verify` (T-5 satırı) | Pencere fonksiyonlu raporun `percentile_disc` ile aynılığı; bozuk zaman damgalarının yakalanıp rapordan dışlanması; tek partisyon taraması; 5M olayda süre |
| Eşzamanlılık | `make test` → `TestIdempotencyClaimIsAtomic`, `TestRefreshTokenRotationAndReuse` | 16 eşzamanlı istekte idempotans anahtarını tek istek alır |
| Kaos | `make chaos` | DB durunca `/health` 503 ve API `503 database_unavailable`; DB dönünce API yeniden başlatılmadan toparlanır; API konteyneri öldürülünce geri gelir |
| Yük | `make load` (Go) ya da `make load-k6` | p50/p95/p99 ve hata oranı tablosu |

## Otomatik testler

```bash
make test-db     # 55432 portunda geçici Postgres 16
make test        # tüm testler, -race, entegrasyon dahil
make cover       # domain paketleri kapsaması (%70 eşiği)
```

91 test fonksiyonu var: 52'si alan paketlerinde (veritabanı gerektirmez), 14'ü gerçek PostgreSQL üzerinde entegrasyon testi (`TEST_DATABASE_URL` yoksa atlanır), geri kalanı JWT, HTTP yardımcıları, SSE hub, metrik ve simülatör testleri. Domain paketlerinin kapsaması %95.6 (CI'da raporlanır ve %70 altı build'i kırar).
