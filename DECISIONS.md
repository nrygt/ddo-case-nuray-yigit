# Mimari karar kayıtları

Her kayıt: bağlam, karar, düşünülen alternatifler, sonuçlar. Sıra, kararların alındığı sıradır.

---

## ADR-001 — Stand tahsisi: açgözlü yerleştirme + tek adımlı takas onarımı

**Bağlam.** Günde ~300 uçuş (~165 turnaround), 26 stand. Kurallar: boyut sınıfı, köprü ihtiyacı, aynı standda iki turnaround arasında 15 dk tampon, aynı uçağın sonraki turnaround'u mümkünse aynı standda. 20 sn içinde çözüm ve çözülemeyenler için gerekçe isteniyor.

**Karar.** Turnaroundlar on-block saatine göre sıralanıp tek tek en iyi puanlı uygun standa yerleştiriliyor. Puan: (onarım modunda) mevcut stand > aynı uçağın önceki standı > en küçük yeterli boyut > köprüyü köprü isteyene bırak > önceki işgalle arası en dar olan stand. Yerleşemeyen her turnaround için "onu engelleyen tek bir turnaround'u başka bir standa kaydırarak yer açabilir miyim?" diye bakılıyor. Yine olmazsa engelleyenlerin listesiyle birlikte `NO_COMPATIBLE_STAND` ya da `ALL_COMPATIBLE_STANDS_OCCUPIED` raporlanıyor.

**Alternatifler.**
- *MILP / CP-SAT:* optimal çözüm verir ama Go'da olgun bir çözücü yok; harici bir süreç (OR-Tools) imaja yüzlerce MB ve bir dil daha ekler. Ayrıca "neden yerleşmedi" sorusuna cevap vermek zorlaşıyor.
- *Tavlama / genetik algoritma:* deterministik olmayan çıktı, testte doğrulaması zor.

**Sonuç.** 300 turnaround ~3 ms, 3000 turnaround ~80 ms. Özdeş makinelerde başlangıca göre açgözlü aralık renklendirme optimaldir; standlar farklı olduğu için burada sezgisel. Rastgele 20 günde "yerleşmeyen turnaround için gerçekten boş uyumlu stand yok" özelliği testle kontrol ediliyor. Bilinen sınırlama: iki adımlı takas gerektiren durumlar kaçabilir (README, soru 2).

---

## ADR-002 — Kuralların son savunma hattı veritabanı

**Bağlam.** Algoritma doğru olsa bile elle atama, eşzamanlı iki yeniden planlama ya da bir hata çakışma üretebilir.

**Karar.** Çakışma `EXCLUDE USING gist (stand_id WITH =, blocked WITH &&) WHERE (active)` ile engelleniyor. `blocked = [on_block, off_block + 15 dk)` aralığını bir `BEFORE` tetikleyicisi hesaplıyor; aynı tetikleyici uçak boyutunu ve köprü ihtiyacını da denetliyor. Ekip ve yakıt aracı çakışmaları da `tasks` üzerinde iki ayrı `EXCLUDE` ile engelleniyor.

**Neden tetikleyici?** Tamponu doğrudan kısıt ifadesine yazmak (`EXCLUDE ... (tstzrange(on_block, off_block + interval '15 minutes') WITH &&)`) PostgreSQL'de `functions in index expression must be marked IMMUTABLE` hatası veriyor: `timestamptz + interval` saat dilimine bağlı olduğu için `IMMUTABLE` değil, üretilmiş sütunda da kullanılamıyor. Tetikleyici bu sınırlamayı aşıyor ve kuralı tek yerde tutuyor.

**Sonuç.** 20 goroutine aynı stand/zaman için aynı anda yazınca tam olarak biri kazanıyor (`TestStandExcludeRace`). API bu hatayı `409 conflict`'e çeviriyor.

---

## ADR-003 — Uçuş beslemesi: tam durum mesajı + uçuş başına sıra numarası

**Bağlam.** Besleme olayları sırasız ve tekrarlı geliyor; sonuç yine tutarlı olmalı.

**Karar.** Her besleme mesajı uçuşun o anki durumunun tamamını taşıyor (fark değil) ve uçuş için artan bir `seq` içeriyor. Kural "en büyük seq kazanır": uçuşun `feed_seq` değerinden küçük/eşit seq'li mesaj `STALE` olarak kaydedilir ama uygulanmaz. Aynı `(source, message_id)` ikinci kez gelirse `UNIQUE` kısıtı yüzünden hiç yazılmaz (`DUPLICATE`). Güncelleme `UPDATE ... WHERE feed_seq < $seq` ile koşullu; satır `FOR UPDATE` ile kilitli.

**Alternatif.** Alındığı zamana göre son yazan kazanır: sırasız gelişte eski bilgi yeniyi ezer. Olayları zaman damgasına göre yeniden oynatmak: daha karmaşık, aynı sonucu veriyor.

**Sonuç.** Uygulama sırası ne olursa olsun son durum aynı (`TestOrderIndependence`, 500 rastgele permütasyon; veritabanında 4 eşzamanlı işçiyle `TestFeedOutOfOrderConcurrentConverges`). Besleme yalnızca uçuşun tahmini saatini değiştirir; planın değişmesi operatörün önizleyip onaylamasıyla olur (ADR-005).

---

## ADR-004 — Çevrimdışı mobil: olay kaynaklı görev geçmişi, sıra bağımsız katlama

**Bağlam.** Ekip liderleri ağ yokken "başladı/bitti" girebilmeli; iki kişi aynı görevi güncelleyebilir.

**Karar.** Sunucuda görev durumu, o görevin tüm olaylarının katlanmasıyla hesaplanıyor; `tasks` satırındaki durum yalnızca önbellek. Katlama kuralları değişmeli, birleşmeli ve idempotent:
durum yalnızca ileri gider (`ASSIGNED < STARTED < DONE`), başlangıç = en erken `STARTED`, bitiş = en erken `FINISHED`. Bitiş başlangıçtan önceyse veya başlangıç yoksa çakışma işaretleniyor. Cihaz her olaya kendisi UUID veriyor; `UNIQUE (client_event_id, occurred_at)` tekrarları engelliyor. Sunucu her olay için `APPLIED`, `MERGED` (başka cihaz daha önce bildirmiş), `DUPLICATE` ya da `REJECTED` döner.

**Alternatifler.**
- *Sürüm numarasıyla iyimser kilit ve çakışmada reddetme:* çevrimdışı çalışan biri için kötü deneyim; saatlerce biriken olaylar toplu reddedilir.
- *Son yazan kazanır:* geç senkronize olan eski bilgi "bitti"yi "başladı"ya geri çevirebilir.

**Sonuç.** Çakışma çözümü kullanıcıya soru sormadan, deterministik. Görev satırı `FOR UPDATE` ile kilitlendiği için iki cihazın eşzamanlı senkronizasyonu da sıraya giriyor (`TestTwoDevicesSyncSameTask`).

---

## ADR-005 — Yeniden planlama saf fonksiyon; önizleme ve uygulama aynı kodu çalıştırır

**Bağlam.** Gecikme zincirleme etki yapıyor; operatör uygulamadan önce ne olacağını görmeli ve plan en az değişiklikle onarılmalı.

**Karar.** `propagation.Replan(timeline, trigger)` veritabanına dokunmayan saf bir fonksiyon. Önizleme bunu okuma bağlantısıyla çağırıp sonucu döner. Uygulama aynı fonksiyonu `SERIALIZABLE` transaction içinde yeniden çalıştırır, sonucun parmak izini önizlemedekiyle karşılaştırır (farklıysa `409 plan_changed`) ve yazar. Stand onarımı ADR-001'deki çözücünün onarım modu: değişmeyen turnaroundlar önce yerlerine oturtulur, sonra kayanlar; böylece taşınan şey her zaman gecikenler olur. Görevler aynı ofsetle kaydırılır ve mümkünse aynı ekipte tutulur.

**Sonuç.** Önizleme ile uygulama arasında "farklı sonuç" riski yok; testler saf fonksiyon üzerinde hızlı koşuyor. Dış meydanda 10 dk toparlanma varsayımı sabit (README, soru 2).

---

## ADR-006 — Bağımlılıkları az tutmak: pgx + x/crypto dışında standart kütüphane

**Bağlam.** HTTP router, JWT ve Prometheus için hazır kütüphaneler var.

**Karar.** Go 1.22 `ServeMux` desenleri (`GET /api/v1/flights/{id}`) yeterli. JWT yalnızca HS256 ve sabit başlık kabul eden ~60 satırlık bir imzalayıcı (algoritma karışıklığı saldırısı mümkün değil). Prometheus için sayaç, gösterge ve histogram yazan küçük bir kayıt defteri. Tek dış bağımlılıklar `pgx` ve `bcrypt` için `x/crypto`.

**Sonuç.** Tedarik zinciri yüzeyi küçük; `api` binary'si 12 MB. Bedeli: JWT ve metrik kodu bizim bakımımızda; ikisi de birim testli.

---

## ADR-007 — Analitik: aylık partisyon + turnaround düzeyinde materialized view + histogram yüzdeliği

**Bağlam.** 5 milyon görev olayında turnaround süresi dağılımı 2 sn altında dönmeli.

**Karar.** `task_events` `occurred_at` üzerinden aylık partisyonlu (budama ile zaman aralıklı sorgular tek partisyona iniyor). `mv_turnaround_durations` turnaround başına bir satır tutuyor (5M olay → ~415 bin satır), kalite sorunlu turnaroundları dışlıyor, `CONCURRENTLY` yenileniyor. Rapor sorgusu yüzdelikleri pencere fonksiyonlarıyla hesaplıyor; ilk sürüm her satıra `row_number()` veriyordu (1.2 sn), son sürüm önce `(grup, süre)` histogramına indirip `sum() OVER (ORDER BY süre)` ile kümülatif adet alıyor (~70–200 ms). Sonuçlar `percentile_disc` ile birebir aynı (testte karşılaştırılıyor).

**Alternatif.** Önceden hesaplanmış günlük yüzdelikler: hızlı ama tarih aralığı değişince yüzdelikler toplanamaz.

**Sonuç.** Rapor hızlı ve tarih aralığı serbest. Bedeli: MV yenilemesi 5M olayda ~11 sn ve tam yenileme (artımlı değil).

---

## ADR-008 — Arayüz: derleme adımı olmayan tek modül + Go statik sunucu

**Bağlam.** Arayüz serbest. Soğuk makinede 5 dk içinde ayağa kalkmalı; ekip lideri ekranı çevrimdışı açılabilmeli.

**Karar.** Framework yok; ~760 satır düz JavaScript (ES modülü), hash tabanlı yönlendirme. Küçük bir Go sunucusu dosyaları gömülü olarak sunuyor ve `/api` isteklerini backend'e aktarıyor (aynı köken, CORS yok, SSE tamponsuz geçiyor). Service worker uygulama kabuğunu önbelleğe alıyor; çevrimdışı olay kuyruğu `localStorage`'da.

**Alternatifler.** React/Vite: daha düzenli bileşen yapısı ama `npm install` soğuk derlemeye dakikalar ekliyor ve imajda Node aşaması gerekiyor. HTMX: sunucu tarafında şablon gerektiriyor, çevrimdışı kuyruk için yine JavaScript yazmak gerekecekti.

**Sonuç.** Web binary'si 6.3 MB (imaj ≈ 8–9 MB), derleme saniyeler. Bedeli: ekran sayısı artarsa elle DOM üretimi zorlaşır.

---

## ADR-009 — Kendi migrasyon çalıştırıcımız

**Bağlam.** Şema değişiklikleri migrasyon dosyalarıyla yapılmalı.

**Karar.** `migrations/*.sql` dosyaları binary'ye gömülü; `migrate` komutu bunları ad sırasıyla, her biri ayrı transaction'da uygulayıp `schema_migrations` tablosuna yazıyor. Aynı anda iki süreç başlarsa `pg_advisory_lock` ile sıraya giriyorlar. Compose'da `migrate` servisi bir kez çalışıp çıkıyor, `api` onun başarıyla bitmesini bekliyor (`service_completed_successfully`).

**Alternatif.** goose/golang-migrate: geri alma (down) desteği var. Bu projede yalnızca ileri migrasyon gerekiyordu; bir bağımlılık ve CLI daha eklemek yerine 60 satır yeterli oldu.

---

## ADR-010 — Tek backend imajı, birden çok binary, distroless

**Karar.** `api`, `migrate`, `simulator`, `verify`, `loadtest` aynı çok aşamalı derlemeden çıkıyor; son aşama `gcr.io/distroless/static-debian12:nonroot` (kabuk yok, uid 65532). Sağlık kontrolü için kabuk olmadığından her binary `-healthcheck` bayrağıyla kendi sağlık ucunu yokluyor. Saat dilimi verisi `time/tzdata` ile binary içine gömülü.

**Sonuç.** Tek derleme, compose'da beş servis. Beş binary toplam 44 MB, distroless taban ~2 MB; tek tek imaja bölmek toplam boyutu küçültmezdi. Kabuk olmadığı için saldırı yüzeyi dar.
