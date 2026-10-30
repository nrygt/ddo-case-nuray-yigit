# PistKoordinat

Bölgesel bir havalimanı için uçuş çizelgesi, stand/kapı tahsisi, yer hizmeti ekip ataması, gecikme yönetimi ve turnaround analitiği.

**Hazırlayan:** Nuray Yiğit · DDO Yazılım Ekibi alım projesi

- Test ve doğrulama rehberi: [TESTING.md](TESTING.md)
- Mimari karar kayıtları: [DECISIONS.md](DECISIONS.md)
- API sözleşmesi: [`backend/openapi/openapi.yaml`](backend/openapi/openapi.yaml) (çalışırken `GET /openapi.yaml`)
- Elle deneme koleksiyonu: [requests.http](requests.http)

## Kurulum

Gereken tek şey Docker (Compose v2).

```bash
git clone <repo-adresi> && cd ddo-case-nuray-yigit
cp .env.example .env
docker compose up --build
```

| Servis | Adres |
|---|---|
| Arayüz | <http://localhost:3000> |
| API | <http://localhost:8080> (`/health`, `/metrics`, `/openapi.yaml`) |
| PostgreSQL 16 | yalnızca compose ağında (`db:5432`), host'a açık değil |

İlk açılışta `migrate` servisi şemayı uygular ve seed'i yükler (≈5 sn), sonra `api`, `simulator` ve `web` sırayla sağlıklı hale gelir. Elle adım yok.

Demo hesapları (parola hepsi için **`Demo.2026!`**):

| Rol | E-posta |
|---|---|
| Operasyon kontrolörü | `ops@pist.test` |
| Yer hizmetleri şefi | `sef@pist.test` |
| Ekip lideri (mobil) | `ekip@pist.test` |
| Havayolu temsilcisi (YK) | `havayolu@pist.test` |
| Yönetim | `yonetim@pist.test` |

Sık kullanılan komutlar:

```bash
make verify       # her özellik ve zorlu görev uçtan uca, PASS/FAIL tablosu (~3 dk)
make reset-demo   # veriyi ilk seed haline döndür
make load         # 60 sn karma yük testi
make chaos        # veritabanını durdurup toparlanmayı göster
make test-db test # Go testleri (entegrasyon dahil)
```

## Mimari

```
                 ┌────────────────────────── docker compose ───────────────────────────┐
  tarayıcı ──▶  web :3000  ── /api, SSE ──▶  api :8080  ──▶  db (PostgreSQL 16)
  (tek ES        (Go, statik dosya +          (Go, net/http)      ▲   ▲
   modülü,        ters vekil)                     ▲               │   │
   service                                        │ HTTP          │   │
   worker)                     simulator ─────────┘               │   │
                               (besleme + tabletler)              │   │
                               migrate (bir kez: şema + seed) ────┘   │
                               verify / loadtest (profil ile) ────────┘
                 └─────────────────────────────────────────────────────────────────────┘
```

Backend tek modül, katmanlar şöyle:

| Paket | İş |
|---|---|
| `internal/domain/allocation` | T-1 stand tahsis çözücüsü (saf Go, veritabanından bağımsız) |
| `internal/domain/propagation` | T-2 gecikme zinciri ve en az değişiklikle yeniden planlama |
| `internal/domain/crew` | T-3 ekip/ekipman atama, sertifika ve dinlenme kuralları, risk |
| `internal/domain/tasksync` | T-4 çevrimdışı olayların sıra bağımsız birleştirilmesi |
| `internal/domain/feed` | Sırasız/tekrarlı besleme kuralları |
| `internal/store` | Tüm SQL; transaction ve kilit yönetimi burada |
| `internal/api` | HTTP uç noktaları; her uç noktanın izinli rolleri tek tabloda (`server.go`) |
| `internal/stream` | SSE yayın merkezi |
| `cmd/*` | `api`, `migrate`, `simulator`, `verify`, `loadtest` |

İş kuralları domain paketlerinde saf fonksiyonlar; böylece hem birim testleri hızlı hem de "ne olur" önizlemesi ile gerçek uygulama aynı kodu çalıştırıyor. Veritabanı ise son savunma hattı: çakışmalar `EXCLUDE`, boyut/köprü kuralı tetikleyici, denetim kaydı değiştirilemez tablo.

### Teknoloji seçimleri

| Katman | Seçim | Neden |
|---|---|---|
| Backend | Go 1.24, standart `net/http` | 1.22 yönlendirme desenleri yeterli; bağımlılık yalnızca `pgx` ve `x/crypto` (ADR-006) |
| Veritabanı | PostgreSQL 16, `pgx/v5` | `EXCLUDE`/`btree_gist`, aralık tipleri, partisyon ve materialized view'lar zorlu görevlerin çekirdeği |
| Frontend | Derleme adımı olmayan düz JavaScript + küçük Go sunucusu | Soğuk derleme saniyeler, imaj küçük, `/api` aynı kökten aktarılıyor (CORS yok, SSE tamponsuz). Ekip lideri ekranı için service worker ile çevrimdışı açılış (ADR-008) |
| Konteyner | Çok aşamalı derleme, `distroless/static:nonroot` | Kabuk yok, root yok; sağlık kontrolleri binary'lerin `-healthcheck` bayrağıyla |

Arayüzün görsel dili apron işaretlerinden geliyor: stand kodları sahadaki sarı stand tabelaları gibi, stand çizelgesindeki sarı çizgi "şimdi". Her ekran yükleniyor / boş / hata durumlarını ayrı çiziyor ve 390 px genişlikte kullanılabiliyor.

### Ortak teknik gereksinimler nasıl karşılandı

| Gereksinim | Uygulama |
|---|---|
| Kimlik / yetki | bcrypt (maliyet 11), HS256 JWT access (15 dk) + DB'de özetle tutulan tek kullanımlık refresh (rotasyon; tekrar kullanımda tüm oturumlar iptal). Her uç noktada rol listesi; 401/403 |
| Migrasyon | `backend/migrations/*.sql`, gömülü, advisory lock'lu çalıştırıcı (ADR-009) |
| Kısıtlar | FK, `UNIQUE`, `CHECK`, 4 adet `EXCLUDE`; para tutarı yok, kanat açıklığı `numeric` |
| Eşzamanlılık | `EXCLUDE` + yarış testleri; `FOR UPDATE` (besleme, senkronizasyon, vardiya); `SERIALIZABLE` + yeniden deneme (planı uygulama); advisory lock (aynı gün için tek tahsis) |
| İdempotans | `Idempotency-Key` → aynı kullanıcı + anahtar için ilk yanıt aynen döner, farklı gövde 422, süren istek 409 |
| Denetim | `audit_log` eklemeli-yalnız; `UPDATE/DELETE` tetikleyiciyle engelli |
| API | `{"error":{"code","message"}}`, katı JSON çözümleme (bilinmeyen alan reddi), keyset sayfalama (`/flights`, `/tasks`) |
| Dayanıklılık | `context` zaman aşımları, düzgün kapanış (SSE akışları önce kapanır), JSON log (parola/token loglanmaz), tüm yapılandırma ortam değişkenlerinden |
| Gözlemlenebilirlik | `/metrics` (istek sayısı/süresi, SSE istemcisi, besleme ve senkronizasyon sonuçları, DB havuzu) |

## Zorlu görevler: kısa özet

| Görev | Yaklaşım | Ölçüm |
|---|---|---|
| T-1 Stand tahsisi | Açgözlü + puanlama + tek adımlı takas onarımı; `EXCLUDE` ile DB garantisi; çözülemeyenler gerekçesiyle | 300 turnaround 2.5 ms çözüm, uçtan uca ~10 ms |
| T-2 Gecikme yayılımı | Saf `Replan`: zincir kaydırma → stand onarımı (değişmeyenler yerinde) → görevleri kaydırıp aynı ekipte tut; önizleme ve `SERIALIZABLE` uygulama, parmak izi kontrolü | Önizleme p50 ~350 ms (yük altında) |
| T-3 Ekip atama | Sertifika günü, vardiya içinde kalma, 11 saat dinlenme, geçiş süresi, 4 yakıt aracı; gecikmeli başlangıç ve risk uyarısı | 528 görev ~200 ms |
| T-4 Canlı + çevrimdışı | Olay kaynaklı geçmiş, sıra bağımsız katlama, cihaz UUID'leriyle tekrar engeli; SSE (Last-Event-ID ile tekrar oynatma) | 2000 olay/dk SSE'de kayıpsız; hub testinde 2000 olay ×5 istemci < 1 sn |
| T-5 Analitik | Aylık partisyon, turnaround düzeyinde MV, histogram + kümülatif pencere yüzdeliği | 5 000 008 olayda rapor 68–200 ms |

## EXPLAIN (ANALYZE) çıktıları

5 000 008 görev olayı, 414 673 turnaround'luk materialized view üzerinde ölçüldü.

**Turnaround raporu (havayolu bazında, tüm zaman):**

```
 WindowAgg (actual time=67.462..67.897 rows=5 loops=1)
   ->  Sort  Sort Key: agg.p90_s DESC
         ->  GroupAggregate  Group Key: cum.grp, cum.n
               ->  Incremental Sort  Presorted Key: cum.grp
                     ->  WindowAgg (actual time=66.592..67.419 rows=342 loops=1)       -- sum(c) OVER (PARTITION BY grp)
                           ->  WindowAgg (actual time=66.501..67.305 rows=342 loops=1) -- kümülatif sum(c) OVER (... ORDER BY duration_s)
                                 ->  Finalize GroupAggregate  Group Key: airline_code, duration_s
                                       ->  Gather Merge  Workers Launched: 1
                                             ->  Partial HashAggregate (rows=342 loops=2)  Memory Usage: 105kB
                                                   ->  Parallel Seq Scan on mv_turnaround_durations (rows=207336 loops=2)
 Execution Time: 68.154 ms
```

Pencere fonksiyonları 415 bin satır yerine 342 satırlık histogram üzerinde çalışıyor; sıralama belleğe sığıyor.

**Zaman aralıklı olay sorgusu, partisyon budaması (10 günlük aralık):**

```
 Aggregate (actual time=46.428..46.431 rows=1 loops=1)
   ->  Append (actual time=3.240..40.043 rows=137805 loops=1)
         Subplans Removed: 17
         ->  Index Only Scan using task_events_202610_task_id_occurred_at_idx on task_events_202610
               Index Cond: ((occurred_at >= (now() - '20 days')) AND (occurred_at < (now() - '10 days')))
               Heap Fetches: 0
 Execution Time: 48.881 ms
```

18 partisyondan 17'si plan aşamasında eleniyor.

Tam çıktılar ve ölçüm betiği: `make verify` T-5 adımı, `TestPartitionPruning`.

## Yük testi

`make load` (Go ile yazılmış, k6 eşdeğeri; `loadtest/k6.js` aynı karışımın k6 sürümü). 50 eşzamanlı kullanıcı, 45 sn, ağırlıklı okuma/yazma karışımı. API, PostgreSQL ve yük üretici aynı 2 çekirdekli makinede çalıştı; ayrı makinelerde sayılar daha iyi olur.

Toplam 15 730 istek, **350 istek/sn**, hata oranı **%0**.

| Uç nokta | İstek | p50 (ms) | p95 (ms) | p99 (ms) | Hata oranı |
|---|---:|---:|---:|---:|---:|
| GET /allocations | 2344 | 74.8 | 131.7 | 162.1 | %0.00 |
| GET /dashboard | 3214 | 275.6 | 377.3 | 427.2 | %0.00 |
| GET /flights | 4735 | 73.4 | 128.4 | 158.4 | %0.00 |
| GET /reports/turnaround | 772 | 250.3 | 364.1 | 442.1 | %0.00 |
| GET /tasks | 1564 | 91.1 | 156.2 | 198.8 | %0.00 |
| POST /notices | 1861 | 2.6 | 35.6 | 60.6 | %0.00 |
| POST /replan/preview | 1240 | 353.3 | 471.9 | 525.6 | %0.00 |
| **Tümü** | 15730 | 91.9 | 364.8 | 444.2 | %0.00 |

Pano en pahalı okuma: tek istekte beş sorgu ve bekleyen yeniden planlama listesi. İlk iyileştirme adayı bunu kısa süreli önbelleğe almak.

## Doğrulama Soruları

### 1. En zorlandığın görev hangisiydi? Hangi alternatifleri düşündün, neden eledin?

**T-2, gecikme yayılımı.** Zor olan kısım gecikmeyi hesaplamak değil, "en az değişiklik"i tanımlamaktı. İlk düşündüğüm, gecikme sonrası günün tahsisini baştan çözmekti; bu kural ihlali üretmiyor ama alakasız onlarca uçağın standını değiştirebiliyor, sahada kabul edilemez. İkinci seçenek değişiklik sayısını en aza indiren bir MILP modeliydi; Go'da olgun bir çözücü yok ve harici bir çözücü imajı ciddi büyütür, ayrıca önizlemede "neden bu değişti" sorusunu açıklamak zorlaşır.

Seçtiğim yol, T-1 çözücüsüne bir "onarım modu" eklemek oldu: zamanı değişmeyen turnaroundlar önce mevcut standlarına oturtuluyor, sonra kayanlar mevcut standlarında kalmayı deniyor, olmazsa başka standa geçiyor. Görevler aynı ofsetle kayıp mümkünse aynı ekipte tutuluyor. Tüm bunlar veritabanına dokunmayan tek bir fonksiyon (`propagation.Replan`), bu yüzden önizleme ile uygulama birebir aynı sonucu veriyor; uygulama `SERIALIZABLE` transaction'da planı yeniden hesaplayıp önizlemedeki parmak iziyle karşılaştırıyor.

T-5'te de bir tur geri döndüm: ilk rapor sorgusu her satıra `row_number()` veriyordu ve 415 bin turnaround'da 1.2 sn sürüyordu (sınırın altında ama pay az). Sürelerin pratikte dakika katları olduğunu fark edince önce `(grup, süre)` histogramına indirip kümülatif toplamı pencere fonksiyonuyla almaya geçtim; aynı sonuç ~70–100 ms.

### 2. Çözümünün kırıldığı ya da yetersiz kaldığı bir senaryo

- **İki adımlı takas gereken tahsisler.** Onarım yalnızca *tek* engelleyen turnaround'u kaydırmayı deniyor. A'yı yerleştirmek için B'yi, B için de C'yi kaydırmak gerekiyorsa A "uyumlu standların hepsi dolu" olarak raporlanır, oysa çözüm vardır. Açgözlü yöntem genel olarak optimal değil; zirve saatlerde stand sayısı sınırdaysa birkaç gereksiz "yerleşemedi" görülebilir.
- **Dış meydan toparlanma varsayımı sabit.** Zincir yayılımında uçağın dış meydanda en fazla 10 dk telafi edebileceğini varsayıyorum; gerçekte tipe ve meydana göre değişir. Ertesi güne sarkan zincir de hesaplanmıyor.
- **Geçiş süresi sabit 5 dk.** Ekibin A1'den R16'ya gitmesi ile yan standa geçmesi aynı sayılıyor; arama 5 dk adımlarla yapılıyor.
- **SSE tek süreç içi.** Yayın merkezi API sürecinin belleğinde. API'yi yatayda çoğaltırsak bir örnekte üretilen olay diğerine bağlı istemcilere ulaşmaz; PostgreSQL `LISTEN/NOTIFY` ya da bir mesaj aracısı gerekir. Halka tampon 4096 olay; daha uzun kopukluklarda istemci kaçırdıklarını alamaz, sayfayı yeniler.
- **Besleme sıra numarasına güveniyor.** Kaynak sistem yeniden başlayıp seq'i sıfırlarsa yeni mesajlar "eski" sayılır.
- **MV tam yenileniyor.** 5M olayda ~11 sn; artımlı değil.
- **Tarayıcıda token `localStorage`'da** ve SSE için sorgu parametresinde. XSS'e karşı `httpOnly` çerez daha güvenli olurdu; sorgu parametresi bizim loglarımıza yazılmıyor ama ara vekiller loglayabilir.

### 3. İki dizin: hangi sorgu için, dizinsiz/dizinli fark

Ölçümler 5 000 008 olaylı veride, `EXPLAIN (ANALYZE)` ile; dizinsiz ölçüm `BEGIN; DROP INDEX ...; EXPLAIN ...; ROLLBACK;` ile alındı.

| Dizin | Sorgu | Dizinli | Dizinsiz |
|---|---|---:|---:|
| `task_events (task_id, occurred_at)` (partisyon başına) | Mobil senkronizasyonda her olaydan sonra görevin geçmişini okuyup katlama: `WHERE task_id = $1 AND event_type IN ('STARTED','FINISHED')` | **0.6 ms** (her partisyonda Index Scan) | **399 ms** (18 partisyonda Parallel Seq Scan) |
| `mv_turnaround_durations (op_date)` | Son 30 günün havayolu raporu | **21 ms** (Bitmap Index Scan, 33 bin satır) | **33 ms** (Parallel Seq Scan) |

İlki kritik: senkronizasyon her olay için bu sorguyu çalıştırıyor, dizinsiz hâliyle 200 olaylık bir tablet partisi dakikalar sürerdi. İkincisinde fark küçük çünkü MV dar (satır başı ~40 bayt) ve paralel tarama hızlı; tarih aralığı daraldıkça fark büyüyor.

Aynı dizin zaman aralıklı sorgularda da işe yarıyor: 10 günlük aralık sorgusu partisyon budamasıyla tek partisyona inip `Index Only Scan` yapıyor (yukarıdaki EXPLAIN).

### 4. Yapay zekâ kullanımı: reddettiğim ya da düzelttiğim çıktılar

Kodun önemli bir kısmını yapay zekâ yardımıyla yazdım; tasarım kararlarını, ölçümleri ve aşağıdaki düzeltmeleri kendim kontrol ettim. Üç örnek:

**a) Yanlış kurulmuş test senaryosu (T-1).** Takas onarımını sınayan ilk test şuydu: C uçağı E standına düşsün, sonra gelen E uçağı onu kaydırsın. Test kırmızıydı. Algoritmaya değil senaryoya baktığımda, C uçağının kayabileceği diğer stand o saatte zaten doluydu; yani çözüm gerçekten yoktu ve algoritma doğru davranıyordu. Testi gerçekten takas gerektiren bir senaryoyla değiştirdim:

```go
// önce: çözümsüz senaryo (stand 2, pinned uçak yüzünden -5'e kadar dolu; A -10'da başlıyor)
p.Turnarounds = append(p.Turnarounds, ta(3, 'C', -60, -20)); p.Pinned[3] = 2
// sonra: köprüsüz A, köprülü C standına düşer; köprü isteyen B gelince A, D standına kaydırılır
Stands: []Stand{stand(1, 'D', false), stand(2, 'C', true)},
Turnarounds: []Turnaround{ta(1, 'C', 0, 60), {ID: 2, Size: 'C', NeedsBridge: true, On: at(10), Off: at(70)}},
```

**b) 125 sahte risk uyarısı (T-3).** İlk sürümde "hedef bitiş" = on-block + hedef turnaround süresiydi. Gece burada kalan uçağın sabah kalkışı için 2 saatlik hazırlık penceresi açtığımda, kalkıştan çok önce biten görevler bile "hedef aşıldı" uyarısı aldı. Uyarı sayısı gerçekçi değildi; kuralı "hedef süre ile kalkıştan 10 dk öncesinin büyüğü" olarak değiştirdim:

```sql
-- önce
t.on_block + make_interval(mins => at.target_turnaround_min)
-- sonra
greatest(t.on_block + make_interval(mins => at.target_turnaround_min), t.off_block - interval '10 minutes')
```

Uyarı sayısı 125'ten 26'ya indi; kalanlar gerçekten kapasite yüzünden geciken görevler.

**c) Veritabanı kesintisinde yanlış hata kodu.** Kaos senaryosunu denerken veritabanı kapalıyken API'nin `500 internal` döndüğünü gördüm; hata işleyici bağlantı hatasını diğer beklenmeyen hatalardan ayırmıyordu. `*pgconn.ConnectError` ve `*net.OpError` için ayrı bir dal ekledim ve sonucu gerçekten veritabanını durdurup açarak doğruladım: kesinti sırasında `503 database_unavailable`, veritabanı dönünce API yeniden başlatılmadan 1–2 sn içinde `200`. Bu sırada bir ölçümüm yanıltıcı çıktı: eski bir API süreci portu tutmaya devam ettiği için yeni binary dinleyemiyordu ve değişikliğim "işe yaramamış" göründü. Bu yüzden ölçümlerden önce çalışan süreci kontrol etmeye başladım.

### 5. Stand tahsisinin doğru çalıştığını nasıl kanıtladım?

Üç katmanda:

1. **Birim testleri ve özellik testi** — kurallar tek tek (14 dk boşluk ret, 15 dk kabul, boyut, köprü, aynı uçak tercihi, pasif stand, sabit atama, takas, onarımda en az değişiklik) ve 20 rastgele 300 turnaround'luk günde iki özellik: çıkan planı bağımsız bir `Validate` fonksiyonu denetliyor, ve yerleşemeyen her turnaround için gerçekten boş uyumlu stand olmadığı kontrol ediliyor.

   ```
   $ cd backend && go test ./internal/domain/allocation -run . -bench Solve -benchtime 50x -v
   --- PASS: TestOverlappingGoToDifferentStands
   --- PASS: TestBufferExactly15MinutesIsAllowed
   --- PASS: TestBuffer14MinutesIsRejected
   --- PASS: TestSizeCompatibility
   --- PASS: TestSmallAircraftPrefersSmallStand
   --- PASS: TestBridgeRequirement
   --- PASS: TestNoCompatibleStandReason
   --- PASS: TestInvalidTimes
   --- PASS: TestSameAircraftKeepsPreviousStand
   --- PASS: TestInactiveStandIgnored
   --- PASS: TestPinnedAllocationIsKept
   --- PASS: TestRepairSwapsBlockingTurnaround
   --- PASS: TestRepairModeMinimalChanges
   --- PASS: TestRandomDaysAreValid (0.05s)
   --- PASS: TestPerformance300FlightsUnder20s
   BenchmarkSolve300-2     50    2465853 ns/op
   BenchmarkSolve3000-2    50   96065066 ns/op
   ok   pistkoordinat/internal/domain/allocation  5.064s
   ```

2. **Veritabanı garantisi** — algoritma hatalı olsa bile çakışma yazılamıyor:

   ```
   $ make test-db && cd backend && TEST_DATABASE_URL=... go test ./internal/store -run 'Exclude|Buffer|SizeAnd' -v
   --- PASS: TestStandExcludeRace (0.10s)          # 20 eşzamanlı yazım, 1 başarı + 19 EXCLUDE hatası
   --- PASS: TestStandBufferEnforcedByDatabase     # 14 dk boşluk 23P01, 15 dk kabul
   --- PASS: TestSizeAndBridgeGuardTrigger         # B77W küçük standa, köprü isteyen köprüsüze yazılamıyor
   --- PASS: TestEquipmentExcludeRace
   ok   pistkoordinat/internal/store  4.409s
   ```

3. **Uçtan uca, çalışan sistemde** — `make verify` tahsisi API üzerinden çalıştırıyor, dönen tahsisleri istemci tarafında stand envanterine karşı yeniden denetliyor (tampon + boyut), elle çakışan atamanın 409 aldığını ve E standları kapatılınca B77W'nin `NO_COMPATIBLE_STAND` gerekçesiyle raporlandığını gösteriyor:

   ```
   F-3     PASS    0.0s  163 aktif tahsis; çakışan elle atama → 409 (EXCLUDE)
   T-1     PASS    0.1s  163/163 yerleşti, çözüm 0.2 ms (uçtan uca 7ms); 15 dk tampon + boyut istemcide doğrulandı; çözülemeyen → NO_C…
   ```

## Bilinen eksikler

- Kapı tahsisi stand üzerinden türetiliyor (köprülü stand → kendi kapısı, uzak stand → otobüs kapısı); otobüs kapılarının kapasitesi ayrıca planlanmıyor.
- Çekme (towing) modellenmedi: gece kalan uçak standını sabaha kadar tutuyor.
- Hava durumu kaynaklı toplu gecikme senaryosu (bonus) ayrı bir ekran olarak yapılmadı; aynı etki besleme uç noktasına toplu tahmini saat göndererek üretilebiliyor.
- `docker compose up` akışı CI'daki `e2e` işinde soğuk başlangıç süresiyle birlikte ölçülüyor; yerel ölçümlerim Docker dışında, PostgreSQL 16 üzerinde yapıldı.
