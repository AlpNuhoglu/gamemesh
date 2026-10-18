# Deployment Faz 1 — Yol Haritası

**Hedef:** `curl https://api.<domain>/health` dışarıdan `200` dönüyor. Arkasında tek
node k3s, cert-manager ile gelen geçerli bir TLS sertifikası ve `main`'e push edilince
GitHub Actions'ın yaptığı gerçek bir deploy var.

**Yaklaşım:** Önce elimizdeki çalışan parçaları sağlamlaştırıyoruz (Bölüm A). Bir sunucuya
dokunmadan önce, sunucuda koşacak her şeyi yerelde, k3s'in aynısı üzerinde deniyoruz.
Sunucu tarafında hata ayıklamak yavaş ve pahalıdır; yerelde ucuzdur.

Her adım şu yapıda:

- **Ne:** yapılacak iş
- **Neden:** gerekçesi ve atlanırsa ne olacağı
- **Nasıl:** dosyalar ve komutlar
- **Öğrenilecek:** adımın öğrettiği kavramlar
- **Bitti sayılır:** doğrulama kriteri. Kriter sağlanmadan sonraki adıma geçmiyoruz.

> **Mevcut yapının incelenmesi (2026-10-08 → 2026-10-13).** Repoda hazır olan 8 parça
> (Dockerfile'lar, docker-compose, health endpoint, graceful shutdown, K8s manifest'leri,
> probe'lar, CI, yerel secret yönetimi) tek tek incelendi ve ölçüldü. Bu incelemeden
> çıkan öneri ve bulgular ilgili adımların altına **"İncelemeden eklenenler"** başlığıyla
> ve kaynak maddesiyle (`M1`…`M8`) birlikte eklendi. Bütün bulguların listesi ve hangi
> adımda ele alındıkları dosyanın sonundaki **[Bulgu dizini](#bulgu-dizini)** tablosunda.
>
> Madde numaraları: **M1** Dockerfile'lar · **M2** docker-compose · **M3** Health endpoint ·
> **M4** Graceful shutdown · **M5** K8s manifest'leri · **M6** Probe'lar · **M7** CI ·
> **M8** Secret yönetimi

---

## Bölüm A — Temeli sağlamlaştır (yerel, sunucu yok)

### A1. Baseline: mevcut stack gerçekten çalışıyor mu?

- [x] **Ne:** `docker compose up --build` ile tüm stack'i kaldırmak, bütün servislerin
  `healthy` olduğunu ve bir uçtan uca isteğin (register → login → leaderboard) çalıştığını
  görmek.
- **Neden:** Değişiklik yapmadan önce "bilinen iyi durumu" kayda geçiriyoruz. Sonradan bir
  şey bozulursa sorunun bizim değişikliğimizden mi yoksa önceden mi geldiğini bu sayede
  ayırt edebiliriz. Bu yapılmazsa her hata iki kat zor ayıklanır.
- **Nasıl:**
  ```bash
  docker compose up --build -d
  docker compose ps                       # STATUS sütununda hepsi "(healthy)"
  curl -s localhost:8080/healthz
  curl -s localhost:8080/api/v1/leaderboard/top/10
  ```
- **Öğrenilecek:** Compose healthcheck'leri, `depends_on: condition: service_healthy` ile
  açılış sırası, container'ların birbirine servis adıyla (`postgres`, `redis`) ulaşması
  (Docker'ın iç DNS'i).
- **Bitti sayılır:** Tüm container'lar healthy, iki `curl` de `200` dönüyor.
- **Sonuç (2026-10-07):** 14 container'ın hepsi ayakta, healthcheck'i olan 11'i healthy,
  restart yok. Register, login, `/players/me` ve leaderboard çalışıyor. Outbox zinciri
  (Postgres → outbox-relay → NATS) çalışıyor. Not: login body'si `identifier` alanı
  istiyor, `email` değil.

### A2. `.dockerignore` ekle

- [x] **Ne:** Build context'ten `.git`, `.env`, `graphify-out/`, `coverage.out`, `bin/`,
  `docs/` ve `scripts/k6/reports/` dizinlerini dışlamak.
- **Neden:** Dockerfile'lardaki `COPY . .` repodaki her şeyi build aşamasına kopyalıyor.
  `.env` (gerçek secret'lar) build cache'ine giriyor. Final imaja girmiyor, çünkü
  multi-stage build sadece binary'yi kopyalıyor, ama cache katmanlarında ve CI'da bu bir
  sızıntı riski. Ayrıca alakasız bir dosya değişince (örneğin bir doc) `COPY . .` katmanı
  geçersizleşiyor ve Go derlemesi baştan çalışıyor.
- **Nasıl:** Repo köküne `.dockerignore` dosyası. Sözdizimi `.gitignore`'a benzer.
- **Öğrenilecek:** Build context nedir, `docker build` daemon'a ne gönderir, katman cache'i
  nasıl geçersizleşir.
- **Bitti sayılır:** `docker build` çıktısındaki "transferring context" boyutu belirgin
  şekilde düşmüş. `docker run --rm --entrypoint ls <build-stage>` ile `.env`'in içeride
  olmadığı doğrulanmış.
- **İncelemeden eklenenler (M1):** Ölçüm: build aşaması **1.064 MB** ve içinde `.env`
  (1054 byte), `.git` (tüm geçmiş), 239 MB Go toolchain'i ve 450 MB modül cache'i var.
  Final imaj 40,6 MB. Doğrulama komutu:
  `docker build --target build -f deployments/docker/Dockerfile.gateway -t gm-buildstage .`
  ve ardından `docker run --rm --entrypoint ls gm-buildstage -A /src`.
- **Sonuç (2026-10-13):** Denylist yerine **allowlist** seçildi (fail-closed): `*`, ardından
  `go.mod`, `go.sum`, `cmd/`, `internal/`, `pkg/` geri açılıyor; `**/*_test.go` hariç.
  Build context **4,55 MB → 254 kB**. Ölçüm yeni bir path'ten yapıldı, çünkü BuildKit
  context'i incremental gönderiyor (aynı dizinde ikinci build 173 kB gösterdi). `/src` 22
  girişten 5'e indi; `.env` ve `.git` yok; 48 üretim `.go` dosyasının 48'i var, `_test.go`
  0. `docs/` ya da yalnızca bir `_test.go` değişince `go build` artık yeniden çalışmıyor
  (8,6 sn → CACHED); gerçek kaynak değişince çalışıyor (kontrol). Not: 1.064 MB olan build
  stage **imajı**; çoğu Go toolchain'i ve modül cache'i, bu adım onu değiştirmiyor. Ayrıca
  ölçüldü: build reproducible (aynı kaynak → aynı binary hash'i), imajda `git` olmadığı için
  binary'ye VCS bilgisi gömülmüyor (`vcs.*` 0 satır; SHA'yı A4'te `ARG VERSION` ile vermek
  şart). 7 servis build oluyor, stack healthy.

### A2a. Yerel geliştirme ortamının hijyeni *(incelemeden eklendi)*

- [x] **Ne:**
  1. **Compose'da portları sadece localhost'a bağlamak (M2):** `"5432:5432"` →
     `"127.0.0.1:5432:5432"`. Aynısı Redis (6379), NATS (4222/8222), Prometheus (9090),
     Grafana (3000), Jaeger (16686) ve OTel (4317/4318) için de geçerli. Gateway (8080)
     da isteğe bağlı olarak localhost'a bağlanabilir. **Presence (8086) ve websocket
     (8084) da listeye girmeli (Bulgu #55):** presence kimlik doğrulaması olmayan bir iç
     API; 2026-10-13'te token'sız `POST /presence/connect` 200 döndü ve rastgele bir
     oyuncu `ONLINE` oldu. README'deki `curl localhost:8086/presence/<id>` örneği çalışmaya
     devam etsin diye yayını kaldırmak yerine `127.0.0.1`'e bağlamak yeterli.
  2. **Redis'e şifre koymak (M2, isteğe bağlı):** `--requirepass ${REDIS_PASSWORD}`.
     `REDIS_PASSWORD` değişkeni config'te zaten var.
  3. **`.env`'i yenilemek (M8):** Senin `.env` dosyan `.env.example`'ın eski bir kopyası;
     15 anahtar eksik (`EVENT_BUS`, `NATS_URL`, `OUTBOX_*`, `OTEL_*`, `REDIS_POOL_SIZE`,
     `SERVICE_VERSION`). Örnekten yeniden kopyalayıp `chmod 600 .env` yapmak (şu an 644,
     yani makinedeki diğer kullanıcılar okuyabiliyor).
  4. **Bayat yorumu düzeltmek (M1):** [Dockerfile.gateway](../deployments/docker/Dockerfile.gateway)
     "final image is ~15 MB" diyor. Ölçülen değer **40–45 MB** (binary ~32–36 MB + Alpine
     ~8 MB).
  5. **`restart:` politikası kararı (M2):** Compose'da hiçbir serviste restart politikası
     yok. Docker Desktop yeniden başladığında stack geri gelmiyor (2026-10-08'de
     gözlemlendi). Seçenekler: bilinçli olarak böyle bırakmak (çöken servisi fark etmek
     için) ya da `restart: unless-stopped`. Kararı verip dokümana yazmak.
     *Veri (2026-10-13):* Docker VM'i (7,7 GiB) stack açıkken paralel build'ler sırasında
     belleği tüketti; Grafana, Prometheus, Jaeger ve OTel Collector `OOMKilled` (137) ile
     öldü. Jaeger ve OTel Collector 12 dk sonra elle kaldırılana kadar trace toplanmadı
     (gateway logunda 48 export hatası), Grafana ve Prometheus hiç geri gelmedi. Aynı gün
     Docker Desktop kapanınca 14 container'ın hiçbiri geri gelmedi. Bilgi (A2a'da
     doğrulanacak): `unless-stopped`, OOM kill'den sonra container'ı yeniden başlatır,
     çünkü OOM kill elle durdurma sayılmaz.
- **Neden:**
  - Ölçüm (M2, 2026-10-10): `lsof` 5432'yi `*:5432` adresinde gösterdi. Mac'in yerel ağ
    IP'si (172.20.10.2, iPhone hotspot ağı) üzerinden 5432 ve 6379'a bağlanılabildi;
    Redis şifresiz `PONG` döndü; macOS firewall kapalıydı. Aynı ağdaki biri, şifresi
    public repoda yazan Postgres'e bağlanabilir ve Redis'i silebilir.
  - Container'lar arası iletişim yayınlanan portları **kullanmaz** (iç ağdaki DNS ile
    `postgres:5432`'ye gider). Bu yüzden localhost'a bağlamak hiçbir servisi bozmaz.
- **Öğrenilecek:** Port yayınlama ile iç ağ farkı, ağ arayüzleri (`lo0`, `en0`),
  `0.0.0.0` bağlamanın anlamı.
- **Bitti sayılır:** `lsof -nP -iTCP:5432 -sTCP:LISTEN` çıktısında `127.0.0.1:5432`
  görünüyor; LAN IP'si üzerinden bağlantı reddediliyor; stack hâlâ healthy.
- **Sonuç (2026-10-14):** Beş madde de yapıldı.
  1. 12 port `127.0.0.1`'e bağlandı (presence ve websocket dahil). Dinleyiciler 12/12
     `*:` → 12/12 `127.0.0.1:`; ev ağında (192.168.1.73) LAN'dan erişim 12/12 → 0/12.
     `localhost` çalışmaya devam ediyor (curl ve nc önce `::1`'i deniyor, reddedilince
     `127.0.0.1`'e düşüyor); README'deki presence `curl`'ü 200.
  2. Redis'e şifre: tek bir anchor (`x-redis-password`) sunucuyu, healthcheck'i
     (`REDISCLI_AUTH`) ve 7 istemciyi besliyor. Şifresiz `PING`/`CONFIG` `+PONG` →
     `-NOAUTH`. Redis kullanan her servis API'si üzerinden denendi (oturum, leaderboard,
     kuyruk, presence, WS); loglarda auth hatası 0. `redis-server` hâlâ `redis`
     kullanıcısıyla çalışıyor.
  3. `.env`: değerlerin hiçbiri özelleştirilmemişti. `.env.bak` (600) yedeği alındıktan
     sonra örnekten yeniden kopyalandı: 25/39 → 39/39 anahtar, izin 644 → 600; compose
     sonucu değişmedi (yeniden oluşturulan container 0). Not: eksik anahtar sayısı
     yeniden ölçülünce 13 çıktı (+ PR #16'da eklenen `TRUSTED_PROXIES`); plandaki 15
     muhtemelen farklı bir sayımdı.
  4. Yorum: gateway imajı 40,5 MB (binary 31,7 + Alpine 8,8).
  5. Karar: 14 serviste `restart: unless-stopped`. Ölçüldü: kendi kendine çıkan
     gateway 0,22 sn'de geri geldi; OOM kill (137, `OOMKilled=true`) ve `exit 3`
     yeniden başlatıldı (ikiye katlanan backoff: +0 +0 +0 +1 +2 +3 +7 +13 sn);
     `docker stop` ve `docker kill` container'ı durdurulmuş bıraktı. Gerekçe: iki günde
     stack üç kez geri gelmedi (4 container OOM; iki Docker Desktop kapanışından sonra
     6 sa ve 11 sa kapalı kaldı). Not: Docker healthcheck'i "unhealthy" container'ı
     **yeniden başlatmaz**, sadece process çıkınca devreye girer; k8s'te bu iş liveness
     probe'unun (A7).
     **Düzeltme (2026-10-15, Bulgu #58):** "Docker Desktop yeniden başlayınca
     `unless-stopped` stack'i geri getirir" iddiası yanlıştı (test edilmeden
     söylenmişti). 14 container 2026-10-14 23:49'da Docker Desktop'la birlikte durdu ve
     daemon yeniden başladığında hiçbiri gelmedi; Docker'ın `config.v2.json`'unda 14'ünde
     de `HasBeenManuallyStopped: true`. Docker Desktop kapanırken container'ları normal
     bir stop isteğiyle durduruyor, `unless-stopped` da elle durdurulanı geri
     getirmiyor. Çökme ve OOM'dan kurtarma ölçüldüğü gibi çalışıyor.
     **Karar (2026-10-15):** `restart: always`. Prod'da compose yok; ama k8s'te
     "elle durdurulmuş" diye bir durum yoktur, controller istenen durumu sürekli uygular
     ve node yeniden başlayınca pod'lar geri gelir. Compose'daki en yakın karşılığı
     `always`; bilerek durdurmak için `docker compose down` (k8s'te `replicas: 0`).
     Docker Desktop yeniden açıldığında stack'in kendiliğinden kalktığı henüz
     doğrulanmadı; bir sonraki açılışta kontrol edilecek.

### A3. Health endpoint'lerini ayır: liveness ≠ readiness

- [x] **Ne:** Şu anki tek `/healthz` yerine üç endpoint:
  - `/livez`: process yaşıyor ve event loop'u tıkanmamış. **Bağımlılık kontrolü yapmaz.**
  - `/readyz`: bu pod trafik almaya hazır mı? Servisin **kendi** zorunlu bağımlılıklarını
    kısa timeout'la (~1 sn) pingler: player ve outbox-relay için Postgres, ilgili servisler
    için Redis, NATS kullananlar için NATS bağlantı durumu.
  - `/healthz`: geriye dönük uyumluluk için `/livez`'in alias'ı olarak kalır (compose
    healthcheck'leri bunu kullanıyor).
  - Gateway'e ek olarak public `/health` route'u (faz hedefi `api.<domain>/health`).
- **Neden:** Kubernetes bu iki probe'a **farklı tepki verir**:
  - Liveness başarısız olursa **container yeniden başlatılır**.
  - Readiness başarısız olursa **pod Service'in endpoint listesinden çıkarılır** ve trafik
    gelmez, ama pod yaşamaya devam eder.

  Liveness'a DB kontrolü koyarsan Postgres 30 saniye kesildiğinde **bütün pod'ların
  hepsi aynı anda restart döngüsüne girer** (cascading failure), oysa DB geri geldiğinde
  kendiliğinden düzelecektiler. Readiness'a hiçbir şey koymazsan DB'ye erişemeyen pod'a da
  trafik gider ve kullanıcı 500 alır. Şu anki kod ikisini de aynı endpoint'e bağladığı için
  ikisi de "process ayakta mı?" sorusundan fazlasını bilmiyor.
- **Önemli tasarım kuralı:** Gateway'in readiness'ı **downstream servislere bakmamalı.**
  Player servisi çökünce gateway de "hazır değil" derse, ingress'in arkasında hiç pod
  kalmaz ve `/health` dahil her şey 503 olur. Gateway hazır değil demek "ben istek
  karşılayamam" demektir; "arkamdaki bir servis bozuk" demek değildir.
- **Nasıl:** [pkg/server/server.go](../pkg/server/server.go) içine `ReadinessCheck`
  fonksiyon listesi alan bir yapı. Her servisin `main.go`'su kendi bağımlılıklarını kaydeder.
  Unit test: bağımlılık hata dönünce `/readyz` 503, `/livez` 200.
- **Öğrenilecek:** Probe semantiği, failure isolation, timeout'lu context ile health check
  (yavaş bir DB'nin probe'u da asmaması için).
- **Bitti sayılır:** Compose'da `docker compose stop redis` yapınca leaderboard'un
  `/readyz`'i 503, `/livez`'i 200 dönüyor. Redis geri gelince `/readyz` tekrar 200 oluyor.
  Testler geçiyor.
- **İncelemeden eklenenler:**
  1. **Kanıt (M2/M3):** Redis durdurulduğunda compose leaderboard'u "healthy" gösterdi,
     gerçek istek 500 döndü. Player durdurulduğunda gateway üzerinden leaderboard 200,
     login 502 döndü (kısmi erişilebilirlik). Bu kısmi erişilebilirlik korunmalı.
  2. **Readiness kuralı (M3):** Readiness, replica'lar arasında yönlendirme kararı içindir.
     Sadece servisin **kendi** zorunlu bağımlılıkları kontrol edilir, başka uygulama
     servisleri asla.
  3. **Health ve metrics isteklerini telemetriden çıkarmak (M3):** Ölçüm: her probe INFO
     seviyesinde loglanıyor (leaderboard'da 30 sn'de 6 satır; k8s'te günde ~360.000
     satıra denk geliyor), `gamemesh_http_requests_total{path="/healthz"}` metriğine
     sayılıyor (RPS'i şişiriyor, p50 gecikmesini olduğundan iyi gösteriyor) ve Jaeger'a
     trace olarak gidiyor (leaderboard için 1 saatte 46 trace; `/metrics` taramaları da
     trace üretiyor). Çözüm: `otelgin.WithFilter`, Logger ve Metrics middleware'lerine
     `/livez`, `/readyz`, `/healthz`, `/health`, `/metrics` için hariç tutma listesi.
  4. **Rate limit muafiyetini açık hale getirmek (M3):** Ölçüm: 300 eşzamanlı istekte
     `/healthz` 300×200, `/api` 188×429 aldı. Muafiyet doğru, ama **sadece satır
     sırasından** kaynaklanıyor ([cmd/gateway/main.go:69-70](../cmd/gateway/main.go#L69-L70);
     gin, middleware zincirini route kaydedildiği anda dondurur). Muafiyeti koda açıkça
     yazmak ve bir test ile korumak gerekiyor; aksi halde bir refactor sessizce bozabilir.
  5. **Public `/health` içeriği (M3):** Minimum bilgi:
     `{"status":"ok","version":"<sha>"}`. Bağımlılık detayları (hangisi bozuk, hata
     mesajları) sadece iç `/readyz`'de olmalı (information disclosure).
  6. **Arka plan işçilerinin canlılığı (M3, bonus soru):** Matchmaking'in eşleştirme
     döngüsü ya da outbox poller durursa HTTP sunucusu çalışmaya devam ettiği için
     `/livez` "ok" der. Çözüm: döngü her turda bir "son çalışma zamanı" yazar; `/livez`
     bu zaman belirli bir eşikten eskiyse 503 döner.
  7. Readiness kontrolleri kısa bir context timeout'u (~1 sn) kullanmalı; yavaş bir
     bağımlılık probe'u asmamalı.
- **Sonuç (2026-10-15):** Yedi commit, hepsi compose üzerinde ölçüldü.
  - `/livez` (bağımlılık yok, döngü heartbeat'leri), `/readyz` (servisin kendi
    bağımlılıkları), `/healthz` = `/livez` alias'ı. Kontroller paralel, tek bir 1 sn
    bütçesi altında; context'i yok sayan bir kontrol bile probe'u asamıyor (test:
    1,00 sn'de 503, "timed out").
  - Readiness tablosu (karar, 2026-10-14): player = Postgres + Redis; matchmaking,
    leaderboard, presence = Redis; websocket = NATS; outbox-relay = Postgres + NATS;
    gateway = yok. Planın "NATS kullananlar için NATS" cümlesi daraltıldı: bu üç
    servisteki NATS publish'leri best-effort (sadece `Warn`), istek başarısız olmuyor.
  - Redis durdurulunca: 4 servis `/readyz` 503 (hata mesajıyla), `/livez` 200, restart
    yok; gateway/websocket/outbox 200; Redis dönünce kendiliğinden 200. NATS
    durdurulunca sadece websocket ve outbox 503 (`nats: RECONNECTING`). Player
    durdurulunca gateway ready kaldı, leaderboard 200, login 502 (madde 1).
  - Heartbeat: `StallAfter = max(30 sn, 3 × aralık)` (sabit 30 sn yerine; aralık
    büyütülürse boşuna restart olmasın). Heartbeat her turda yazılıyor, tur başarısız
    olsa da: Redis 40 sn kapalıyken 8 eşleştirme turunun 8'i de hata verdi, `/livez` 200
    kaldı, restart 0.
  - Telemetri (60 sn, leaderboard): sayaç `/healthz` +12 → +0, `/metrics` +14 → +0;
    probe log satırı 28 → 0; `/healthz` trace'i 11 → 0; gerçek istekler kayıtlı.
    Sapma (bildirildi): başarısız probe'lar loglanmaya devam ediyor, çünkü hangi
    bağımlılığın bozulduğu başka hiçbir yerde görünmüyor. Ölçüm notu: Jaeger API'si
    `lookback`'i yok sayıyor, pencere `start`/`end` ile verilmeli.
  - Rate limit: muafiyet `RateLimit` içinde; route'u limiter'dan sonra kaydeden test eski
    kodda kırmızıydı (`/readyz` 429). Public `/health` bilinçli olarak limitli (300
    istekte 115×200 / 185×429).
  - Public `/health`: `{"status":"ok","version":"<SERVICE_VERSION>"}`, başka alan yok.
  - Compose healthcheck'leri `/readyz`'e geçti: Redis kapalıyken 4 servis 49 sn'de
    unhealthy, restart yok, Redis dönünce 5 sn'de 11/11 healthy.
  - k8s manifest'lerindeki probe'lar hâlâ `/healthz` kullanıyor; A7'de `/livez` ve
    `/readyz`'e taşınacak.

### A3a. Kapanış davranışını tamamla *(incelemeden eklendi)*

- [x] **Ne:**
  1. **WebSocket istemcilerine kapanışı bildirmek (M4):** `srv.RegisterOnShutdown` ile
     `hub.CloseAll()` çağırmak. Bu fonksiyon her istemcinin `send` kanalını kapatır;
     [client.go:98-101](../internal/wsgateway/client.go#L98-L101)'deki mevcut kod
     zaten close frame gönderiyor. `RegisterOnShutdown` fonksiyonları beklenmediği için
     `CloseAll` içinde bir `WaitGroup` ile frame'lerin gönderilmesi beklenmeli.
  2. **Close frame'e kod eklemek (M4):** Şu anki kod boş bir close frame gönderiyor
     (`[]byte{}`), bu yüzden istemci **1005** görür. Doğrusu:
     `websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down")`
     (**1001**).
  3. **Arka plan işçilerinin bitmesini beklemek (M4):** Matchmaking döngüsü, outbox relay,
     WS bridge ve heartbeat `ctx` ile durduruluyor ama `main` onları beklemiyor
     (`WaitGroup` yok). HTTP kapandıktan sonra işçiler için kısa bir bekleme eklemek.
  4. **Timeout'ta `defer`'lerin atlanmasını önlemek (M4):** `Shutdown` 15 sn'yi aşarsa
     `main` `log.Fatal` çağırıyor; `os.Exit` olduğu için trace flush ve `log.Sync`
     atlanıyor. Hatayı loglayıp normal şekilde dönmek.
  5. **Açılışta bağımlılığa yeniden bağlanma (M2, isteğe bağlı):**
     [cmd/outbox-relay/main.go:51](../cmd/outbox-relay/main.go#L51) Postgres'e ilk
     denemede bağlanamazsa `log.Fatal` ile çıkıyor. Kısa bir backoff ile birkaç deneme,
     k8s'teki CrashLoopBackOff gürültüsünü azaltır.
     *Ek (Bulgu #57, 2026-10-15):* Postgres DSN'inde `connect_timeout` ve
     `statement_timeout` yok. Bir ağ bölünmesinde sorgular uzun süre asılı kalabilir:
     outbox döngüsünün heartbeat'i bayatlar ve container yeniden başlatılır (takılmış bir
     bağlantı için makul ama sınırı açık değil), açılıştaki bağlantı denemesi de asılı
     kalabilir. Redis çağrıları go-redis'in varsayılan timeout'larıyla zaten sınırlı.
  6. **Doğrulanması gereken (M4):** Matchmaking döngüsü kuyruktan oyuncu alıp oda
     oluştururken yarıda kesilirse ne oluyor? İşlemin atomik olup olmadığı kodda
     doğrulanmadı.
  7. **Gözlem (2026-10-13/14, incelenmedi):** Docker Desktop kapanırken 14 container'dan
     yalnızca NATS **exit 1** ile çıktı, diğerleri 0; dört kapanışın dördünde de (4/4)
     tekrarlandı. JetStream'in temiz kapanıp kapanmadığı ve k8s'te bunun bir etkisi
     olup olmadığı incelenmeli.
     *Çözüldü (2026-10-15):* Tek başına bir NATS container'ıyla denendi: SIGTERM'de
     loglar "JetStream Shutdown → Server Exiting" gösteriyor (temiz kapanış) ve exit 1;
     SIGINT'te aynı temiz kapanış exit 0. Bu bir arıza değil, nats-server'ın SIGTERM
     çıkış kodu. Dev'i prod'dan ayırmamak için compose'a `stop_signal` eklenmedi. k8s
     için asıl konu NATS'ın lame duck modu (A7).
- **Neden:**
  - Ölçüm (M4, 2026-10-11): Websocket container'ı durdurulduğunda sunucu 0,19 sn'de exit 0
    ile kapandı, ama bağlı istemci **1006 (Abnormal Closure)** aldı. Her deploy'da o
    pod'daki bütün oyuncuların bağlantısı kopuyor. Hepsi aynı anda yeniden bağlanmaya
    çalışıyor (thundering herd) ve kopma sırasında gönderilen event'ler kayboluyor.
  - Laboratuvar deneyi: `http.Server.Shutdown` listener'ı hemen kapatıyor, boştaki
    bağlantıları kapatıyor, devam eden istekleri bitiriyor, ama **hijacked bağlantıları
    hiç izlemiyor** (Go dokümantasyonu: *"Shutdown does not attempt to close nor wait for
    hijacked connections such as WebSockets"*).
- **Öğrenilecek:** HTTP hijack, WebSocket kapanış protokolü (1000/1001/1005/1006),
  `RegisterOnShutdown`, kapanış sırası.
- **Bitti sayılır:** Bağlı bir WS istemcisi container durdurulduğunda **1001** alıyor.
  Kapanış loglarında arka plan işçilerinin durduğu görülüyor.
- **İstemci tarafı notu (kapsam dışı):** İstemciler 1001'de rastgele bir beklemeyle
  (jitter) hemen yeniden bağlanmalı ve bağlandıktan sonra durumu yeniden çekmeli (resync,
  örneğin `GET /queue/status`).
- **Sonuç (2026-10-15):** Beş commit, compose üzerinde ölçüldü.
  1. `server.Run` kapanış hook'larını `Shutdown` ile birlikte aynı 15 sn bütçesinde
     çalıştırıp bekliyor (`RegisterOnShutdown`'ın fonksiyonları beklenmediği için
     yeterli değildi). `hub.CloseAll` (planda var sanılıyordu, yoktu; yazıldı) her
     istemciye `1001 "server shutting down"` gönderiyor, cevap gelmezse bütçe sonunda
     bağlantıyı kesiyor. Node istemcisi: **1006 → 1001**, kapanış 0,30 sn.
  2. Close frame kodu: 1001 artık `WriteControl` ile `CloseAll`'dan gidiyor; writePump'ın
     boş frame'i sadece istemci zaten gitmişken (unregister) çalışıyor.
  3. Arka plan işçileri: eşleştirme turu ve outbox poll'u `context.WithoutCancel` + 10 sn
     ile çalışıyor; kapanış yeni tur başlatmıyor ama devam edeni bitiriyor. `main`
     işçileri en fazla 5 sn bekliyor (`server.Workers`). Testler tur ortasında iptal
     ediyor; eski kodda ikisi de `context canceled` ile kırmızıydı. Üç servis 0,16–0,20
     sn'de exit 0 ile kapandı, son log satırı "background workers stopped". Bütçe:
     preStop 5 + HTTP 15 + işçiler 5 = 25 sn < 30 sn.
  4. `main`'ler `os.Exit(run())`: kapanış hatası `defer`'leri atlamıyor, exit kodu yine 1.
     Açılıştaki `log.Fatal`'lar bilinçli olarak bırakıldı.
  5. Açılışta bağlantı ve #57: DSN'lere `connect_timeout=5` (ulaşılamayan host'a deneme
     75,0 sn → 5,0 sn); `database.Open` 30 sn boyunca backoff ile deniyor. Postgres
     kapalıyken yeniden başlatılan player 5 deneme yapıp Postgres açılınca bağlandı,
     restart yok. Global `statement_timeout` konmadı (migration'ları keser).
  6. Matchmaking'in yarıda kesilmesi (kod okuması + test): tur atomik değil ama
     idempotent tasarlanmış (Snapshot okur, sonra oda kaydet → kuyruktan çıkar → event).
     En kötü kesilme "çıkarıldı ama bildirilmedi" anı; planlı kapanışta artık olmuyor
     (madde 3). SIGKILL'de kalan boşluk Faz 1 sonrasına yazıldı.
  7. NATS exit 1 açıklandı (yukarıda).

### A3b. WebSocket oturum güvenliği *(incelemeden eklendi, canlıya çıkmadan önce zorunlu)*

- [x] **Ne:**
  1. [internal/wsgateway/handler.go](../internal/wsgateway/handler.go) bağlantı kurulurken
     token'ın imzasına ek olarak oturumun Redis'te aktif olup olmadığını kontrol etmeli
     (gateway'in kullandığı `SessionChecker` ile).
  2. Logout bir event yayınlamalı. Websocket servisi bu event'i alınca o oyuncunun açık
     bağlantılarını kapatmalı.
- **Neden (M8, canlı test 2026-10-13):**
  - Logout yapılmış bir token, logout'tan 6 sn sonra gateway'de **401** aldı (5 sn'lik
    oturum önbelleği bilinçli ve belgelenmiş bir tercih), ama websocket'e **bağlanabildi.**
  - Ingress `/ws` trafiğini gateway'e değil doğrudan websocket servisine gönderiyor
    ([20-ingress.yaml](../deployments/k8s/20-ingress.yaml)). Gateway'deki oturum kontrolü
    burada hiç devreye girmiyor.
  - Sonuçları: logout WS erişimini kapatmıyor; çalınan bir token 24 saat (`JWT_EXPIRY`)
    boyunca iptal edilemiyor. JWT secret sızarsa sahte token'lara karşı ikinci savunma
    hattı yok: gateway'de sahte `jti` Redis'te bulunamadığı için reddedilir, websocket'te
    ise kabul edilir.
- **Öğrenilecek:** Stateless JWT ile server-side oturum iptali arasındaki gerilim,
  savunma derinliği (defense in depth).
- **Bitti sayılır:** Logout'tan sonra aynı token ile WS bağlantısı 401 alıyor. Açık bir WS
  bağlantısı logout'tan sonra kapanıyor.
- **Sonuç (2026-10-16):** Üç commit, compose üzerinde ölçüldü. Karar (2026-10-16): event
  **ve** periyodik tarama birlikte (seçenek c).
  1. El sıkışmada önbelleksiz Redis oturum kontrolü (fail closed, 2 sn sınır). Logout
     edilmiş token'la yeni WS: kabul → **401**. Gerçek secret'la imzalanmış ama uydurma
     `jti`'li token: gateway 401, websocket 401 (secret sızıntısına karşı ikinci hat).
     Redis kapalıyken el sıkışma 0,15 sn'de 503; websocket `/readyz` artık Redis'i de
     kontrol ediyor. `REDIS_POOL_SIZE` websocket'e de bağlandı.
     *Bağlantı fırtınası (kullanıcının işaret ettiği risk):* tek istemciden aynı anda 2000
     ve 5000 el sıkışma; havuz 140 (varsayılan, 14 CPU), 20 ve 2 ile hepsi açıldı, hata
     0 (havuz 2'de 5000 el sıkışma 1,21 sn, p95 965 ms; 140'ta 824 ms). Aynı node'daki bir
     `EXISTS` milisaniyenin altında, yani birkaç bağlantı saniyede binlerce kontrolü
     taşıyor (Little yasası). Risk, Redis gecikmesinin yükseldiği durumda; 2 sn'lik sınır
     o zaman hızlı 503 ve istemci jitter'ı sağlıyor.
  2. Her 10 sn'de tarama (1000'lik Redis pipeline'ları): oturumu silinmiş ya da JWT'si
     süresi dolmuş (Bulgu #59) bağlantılar `1008` ile kapanıyor. Redis hatasında **fail
     open** (bağlantılar korunuyor). Logout sonrası açık WS: 8 sn sonra hâlâ açık →
     **7,5–7,7 sn**'de `1008 "session revoked"`.
  3. Logout outbox'a `SessionRevoked{jti}` yazıyor; bridge sadece o `jti`'nin bağlantısını
     kapatıyor (oyuncunun diğer cihazları açık kalıyor). Logout → kapanış **0,3–0,9 sn**.
     Event yazılamazsa logout yine başarılı, tarama 10 sn içinde kapatıyor.
  - İstemci sözleşmesi `docs/api.md`'de: 1001 → jitter'la yeniden bağlan; 1008 → aynı
    token'la bağlanma, tekrar login ol.
  - Gözlem: gateway logout'tan hemen sonra hâlâ 200, 6 sn sonra 401 (belgelenmiş 5 sn'lik
    önbellek). Faz 1 sonrasına iyileştirme olarak yazıldı.
  - Birden fazla websocket replica'sında event bunlardan sadece birine gider (Bulgu #29);
    A7'ye kadar diğerlerini tarama kapsıyor.

### A4. Dockerfile'ları üretime hazırla

- [x] **Ne:**
  1. Go build'e BuildKit cache mount'ları eklemek
     (`--mount=type=cache,target=/go/pkg/mod` ve `/root/.cache/go-build`).
  2. `ARG TARGETOS TARGETARCH` ile cross-compile desteği (`GOOS/GOARCH`).
  3. `ARG VERSION` ile binary'ye ve `SERVICE_VERSION`'a commit SHA'sını gömmek.
  4. `USER 10001:10001` şeklinde **sayısal** kullanıcı.
  5. Base imajları sabit sürüme pinlemek (zaten `alpine:3.20` gibi pinli, kontrol et).
  6. *(İsteğe bağlı)* 7 neredeyse aynı Dockerfile yerine `ARG SERVICE` alan tek bir
     Dockerfile.
- **Neden:**
  - Mac'in **arm64**, Hetzner'in ucuz CX serisi ise **amd64**. Mac'te build edilen imaj
    VPS'te `exec format error` verir. `TARGETARCH` ile Go, emülasyon olmadan doğrudan hedef
    mimari için derler (planda "QEMU'dan 10 kat hızlı" deniyordu; bu makinede ölçülen 4,3 kat, bkz. Sonuç).
  - Kubernetes'te `runAsNonRoot: true` kullanıldığında kubelet, kullanıcının root olmadığını
    imajdaki **sayısal** UID'den anlayabilir. `USER app` isim olduğu için doğrulayamaz ve
    pod'u reddeder.
  - SHA'yı imaja gömmek, canlıdaki `/health` yanıtına bakarak "hangi commit çalışıyor?"
    sorusunu cevaplamayı sağlar.
- **Öğrenilecek:** BuildKit, multi-platform build, `docker buildx`, katman cache'i ile
  build cache'i arasındaki fark.
- **Bitti sayılır:**
  `docker buildx build --platform linux/amd64,linux/arm64 -f ... .` başarılı oluyor ve
  `docker run --rm <imaj>` amd64 imajını (emülasyonla) çalıştırıp `/livez` 200 dönüyor.
- **İncelemeden eklenenler:**
  1. **Desteklenen sürümlere geçmek (M1, M7):** Go yalnızca son iki ana sürüme güvenlik
     desteği verir. Mac'te `go1.27.1` kurulu olduğuna göre 1.27 yayınlanmış, yani servislerin
     derlendiği **Go 1.25 artık desteklenmiyor.** Alpine 3.20'nin destek süresinin de 2026
     baharında dolmuş olması beklenir; resmi takvimden doğrulanmalı. Servisler desteklenen
     bir Go sürümüne (1.26 ya da 1.27) ve desteklenen bir Alpine sürümüne taşınmalı.
     *Ara not (2026-10-13):* Go 1.26 geçişi Bulgu #51 yüzünden `fix/deps-x-net` ile öne
     çekildi: `go.mod` `go 1.26.0`, Dockerfile'lar `golang:1.26-alpine`, CI ve govulncheck
     `GO_VERSION: "1.26"`. A4'te kalanlar: `toolchain` direktifi, 1.26 mı 1.27 mi kararı,
     Alpine sürümü, digest pinleme.
  2. **Go sürümünü her yerde hizalamak (M7):** Şu an `go.mod` 1.25.0, CI "1.25",
     Dockerfile `golang:1.25-alpine` (1.25.14), govulncheck "1.26", Mac 1.27.1. `go.mod`'a
     `toolchain` direktifi eklenip CI ve Dockerfile aynı sürüme sabitlenmeli. Bu, A5a'daki
     govulncheck sorununu da çözer.
  3. **Digest ile pinleme (M1, isteğe bağlı):** `alpine@sha256:...` ile tam tekrarlanabilir
     build, güncellemeler için Renovate ya da Dependabot.
  4. `CGO_ENABLED=0` açıkça kalmalı (M1). `golang:*-alpine` imajında gcc olmadığı için CGO
     zaten kendiliğinden kapanıyor, ama açık bayrak, biri ileride gcc eklerse build'in
     sessizce dinamik binary'ye dönmesini önlüyor. (Deneyle gösterildi: Debian'da CGO açık
     derlenen bir binary Alpine'da `exec /app: no such file or directory` ile açılmıyor.)
- **Sonuç (2026-10-16):** Üç commit. Kararlar (2026-10-16): Go 1.26 + `toolchain` sabiti,
  Alpine 3.24, sürüm ldflags ile, tek Dockerfile, yerel çok platformlu doğrulama için
  `docker-container` builder (`gamemesh-multi`), digest sabitleme A5a'ya.
  1. `go.mod`: `toolchain go1.26.9`; CI `go-version-file: go.mod` ile okuyor. Bunun için
     `setup-go` v5 → **v6** yükseltildi: v5'in kaynağında `toolchain` hiç geçmiyor (sadece
     `go 1.26.0` satırını okurdu), v6 okuyup `GOTOOLCHAIN=local` set ediyor. govulncheck de
     aynı dosyadan okuyor; artık gönderilen toolchain'i denetliyor (yeni bir stdlib yaması
     job'u kırmızıya çevirir; doğru sinyal). Build imajı `golang:1.26.9-alpine3.24`.
     **Düzeltme (2026-10-16, A5):** govulncheck kısmı çalışmamıştı. PR #22 sonrası `main`'de
     `test` job'u `go1.26.9`, `security` job'u `go1.27.2` kurdu; logda "Both go-version and
     go-version-file inputs are specified, only go-version will be used" uyarısı var.
     Sebep: action `go-version-input`'a varsayılan `stable` veriyor. A5'te
     `go-version-input: ""` ile düzeltildi.
     Resmi takvim: Go 1.25 desteksiz (1.27.0, 2026-08-19); Alpine 3.20'nin desteği
     2026-04-01'de bitti, 3.21'inki 2026-11-01'de bitiyor.
  2. Tek `deployments/docker/Dockerfile` (`--build-arg SERVICE=`), 7 dosya silindi (yorum,
     ad ve port dışında birebir aynıydı). Build aşaması `$BUILDPLATFORM`'da koşup
     `GOOS/GOARCH` ile cross-compile ediyor; final aşamada `RUN` yok; `USER 10001:10001`;
     Alpine 3.24.2. Ölçüldü: amd64 + arm64 build başarılı, OCI index'te iki manifest de
     `user=10001:10001`; amd64 imajı emülasyonla `x86_64`, uid 10001 olarak çalıştı ve
     `/livez` 200 (42,7 MB). Soğuk amd64 derleme: QEMU altında 35,3 sn → cross-compile
     8,2 sn (**4,3×**). Tek satırlık değişiklikten sonra `go build`: 8,1 sn → 0,6 sn.
  3. Commit SHA `-ldflags -X` ile binary'de; compose `SERVICE_VERSION` vermiyor,
     `VERSION` build arg'ı veriyor; CI `github.sha` veriyor. `/health` →
     `{"status":"ok","version":"8aefb98"}`. Sadece `VERSION` değişince `go build` 0,6 sn
     (sadece link).
  - A6 için not (bilgi, test edilmedi): BuildKit cache mount'ları `cache-to: type=gha`'ya
    aktarılmaz; CI'da her koşu temiz bir builder açtığı için cache mount'ların faydası
    CI'da görünmeyebilir. Katman önbelleği yine çalışır.

### A5. CI'daki servis listesi tutarsızlığını gider

- [x] **Ne:** [ci.yml](../.github/workflows/ci.yml) içindeki `build` ve `docker` job'larına
  `presence` ve `outbox-relay` servislerini eklemek. Servis listesini tek bir yerde tutmak.
- **Neden:** Şu an CI 7 servisin 5'ini derliyor. `presence` ya da `outbox-relay` bozulsa CI
  yeşil kalır, hata deploy anında ortaya çıkar. "CI yeşil" bilgisinin anlamlı olması için
  her şeyi kapsaması gerekir.
- **Öğrenilecek:** GitHub Actions matrix stratejisi, job bağımlılıkları (`needs`).
- **Bitti sayılır:** PR'da 7 docker job'u görünüyor ve hepsi yeşil.
- **İncelemeden eklenenler (M7):**
  1. **`build` job'unu kaldırmak ya da sadeleştirmek:** Test job'u `./cmd/...` dahil her
     şeyi zaten derliyor, docker job'u binary'leri yeniden derliyor ve yüklenen binary'leri
     hiçbir job kullanmıyor.
  2. **Docker `.tar` artifact'larını yüklemeyi bırakmak:** ~40 MB × 5 dosya yükleniyor ama
     hiçbir job kullanmıyor. A6'da registry'ye push ile değiştirilecek.
  3. **`concurrency`:** CI için
     `group: ci-${{ github.ref }}, cancel-in-progress: true` (aynı branch'e art arda gelen
     push'larda eski çalıştırmaları iptal eder). Deploy için tersi gerekli (D2).
  4. **`permissions: contents: read`** workflow seviyesinde. Yazma yetkisi sadece ihtiyaç
     duyan job'a verilir (A6'da docker job'una `packages: write`).
- **Yapılanlar (2026-10-16), CI kanıtı bekleniyor:** Kararlar (2026-10-16): liste `cmd/`'den
  otomatik; `build` job'u kaldırıldı; imaj ve binary artifact'ları kaldırıldı (coverage
  kaldı); `cancel-in-progress` sadece PR'larda; workflow seviyesinde `contents: read`.
  - Önce (`main`'deki son push, 338 sn): 5/7 servis; `build` job'u 73 sn; kullanılmayan
    artifact'lar ~76 MB imaj tar'ı + 62 MB binary; `security` +296 sn'de başlıyor.
  - Yerel kanıt: `cmd/presence`'a bilerek konan derleme hatasında `go test ./cmd/...`
    `FAIL … [build failed]` verdi (`build` job'u gereksiz). Keşif komutu 7 servisi
    listeliyor. `actionlint` temiz. Repo'nun varsayılan token yetkisi zaten `read`.
  - Plan maddesi 3'ten sapma: `cancel-in-progress` sadece PR'larda; `main`'de iptal edilen
    bir çalıştırma, aynı workflow'daki deploy'u yarıda kesebilir.
  - #25 düzeltmesi bu adıma alındı (bkz. A4 Sonuç).
- **Sonuç (2026-10-16), PR #23'ün CI'ı:** 7 `docker` job'unun 7'si yeşil (presence ve
  outbox-relay dahil); `build` job'u yok; artifact olarak sadece `coverage` var.
  `security`: `Setup go version spec 1.26.9`, uyarı yok (#25 gerçekten çözüldü). Token
  yetkileri: `Contents: read`, `Metadata: read`. Toplam süre 338 sn → 280 sn (önceki
  ölçüm `main`'deydi ve 6 sn'lik deploy'u içeriyordu); `docker` job'ları 91–124 sn → 70–86
  sn. `concurrency`: PR'a ikinci push gelince ilk çalıştırma `cancelled` oldu (`test`,
  `docker`, `security` yarıda kesildi).

### A5a. CI güvenlik taramalarını sıkılaştır *(incelemeden eklendi)*

- [x] **Ne:**
  1. **`security` job'undaki `needs: docker` bağımlılığını kaldırmak:** Trivy imajları
     değil dosya sistemini tarıyor; docker'ı beklemesi kritik yola yaklaşık 2 dk ekliyor.
  2. **İmaj taraması eklemek:** Build edilen imajlara `trivy image` çalıştırmak. `fs`
     taraması Alpine'ın işletim sistemi paketlerine (busybox, musl, ssl) bakmıyor; Alpine
     sürümünün destek dışı olması bu yüzden yakalanmıyor.
  3. **Trivy'ye `scanners` parametresini açıkça vermek:** Step'in adı "deps + misconfig",
     ama `scanners` belirtilmemiş. Misconfig taramasının gerçekten çalışıp çalışmadığı
     job loglarından doğrulanmalı.
  4. **govulncheck'in production'daki Go sürümünü denetlemesi:** Şu an Go 1.26 ile
     çalışıyor, servisler 1.25 ile derleniyor. govulncheck stdlib açıklarını kendi çalıştığı
     Go sürümüne göre raporladığı için, 1.25'te olup 1.26'da düzeltilmiş açıklar
     görünmüyor. A4'teki sürüm hizalaması bunu çözüyor.
  5. **Taramaların PR'ları nasıl etkileyeceğine karar vermek:** CI geçmişindeki 4 kırmızı
     çalıştırmanın hepsi `security` job'undan geliyor (2 Trivy, 2 govulncheck) ve ikisi
     **sadece doküman değiştiren** PR'lardı. Seçenekler: zamanlanmış gece taraması
     (`schedule:`) ile issue açmak, `.trivyignore` içinde gerekçesi ve son kullanma tarihi
     olan istisnalar tutmak, Dependabot/Renovate ile bağımlılık güncellemelerini
     otomatikleştirmek.
  6. **Action'ları commit SHA'sı ile sabitlemek:** `actions/checkout@v4` →
     `actions/checkout@<sha> # v4.x.y`, güncellemeler Dependabot (`github-actions`
     ekosistemi) ile. Gerekçe: tag'ler taşınabilir (Mart 2025'teki `tj-actions/changed-files`
     olayında tag'ler kötü amaçlı bir commit'e yönlendirildi ve CI loglarına secret'lar
     sızdı).
  7. **kubeconform'u sabit bir sürümden ve checksum doğrulamasıyla indirmek:** Şu an
     `latest` sürüm, checksum kontrolü olmadan `curl` ile indiriliyor.
  8. **Secret taraması:** CI'a gitleaks eklemek ve GitHub'da secret scanning + push
     protection'ı açmak (public repolar için ücretsiz). Bkz. A9.
  9. **Bir taramanın başarısızlığı diğerini gizlememeli (Bulgu #54):** Trivy ve
     govulncheck aynı job'da sırayla çalışıyor. 2026-10-13'te Trivy kırmızıyken
     govulncheck hiç çalışmadı ve erişilebilir bir açık (GO-2026-6505) Trivy düzeltilene
     kadar görünmedi. Taramalar ayrı job'lara bölünmeli ya da `if: always()` ile hepsi
     çalıştırılmalı.
- **Öğrenilecek:** Tedarik zinciri güvenliği, reachability analizi ile sürüm eşleştirme
  farkı, güvenlik kapılarının zamana bağlı doğası.
- **Bitti sayılır:** `security` job'u docker'ı beklemiyor; imaj taraması çalışıyor; bütün
  action'lar SHA ile sabitli; Dependabot yapılandırması var.
- **Yapılanlar (2026-10-17), CI kanıtı bekleniyor:** On commit. Kararlar (2026-10-17):
  taramalar ayrı job'lar; imaj taraması bloklayan ve EOL kontrollü; misconfig A7'ye kadar
  sadece rapor; gece taraması; doküman PR'ları muaf tutulmadı; `.trivyignore` şimdilik yok
  (gerekirse gerekçe + `exp:` tarihiyle); action'lar SHA, imajlar digest ile; Dependabot.
  1. `security` → `scan-deps` (Trivy fs), `govulncheck`, `gitleaks`: hiçbirinin `needs`'i
     yok. `deploy`'un `needs`'ine `docker` açıkça eklendi (önceden `security` üzerinden
     dolaylıydı).
  2. Trivy `scanners` açıkça veriliyor: `vuln,secret` bloklayan; `misconfig` ayrı step,
     `exit-code: 0`, bloklayandan önce. Yerel (Trivy 0.70.0, action'ın varsayılanı):
     "Misconfiguration scanning is enabled", 33 HIGH, hepsi `deployments/k8s` (KSV-0118
     ×22, KSV-0014 ×11). Yeni Bulgu #60.
  3. Her `docker` matrix job'u imajı `load` edip `trivy image` çalıştırıyor (HIGH/CRITICAL,
     düzeltmesi olanlar, bloklayan, `TRIVY_EXIT_ON_EOL=1`). Yerel: `alpine:3.20` EOSL ve
     0 bulgu; ayarsız exit 0, ayarla exit 1. `alpine:3.24.2` ve gateway imajı exit 0 (3.24
     Trivy'nin EOL listesinde henüz yok, sadece uyarı).
  4. Gece 03:17 UTC `schedule` + `workflow_dispatch`. İki koruma (GitHub belgelerinden,
     CI'da henüz test edilmedi): `deploy` sadece `push`'ta çalışıyor (schedule'ın ref'i de
     `main`; eski koşulla her gece deploy olurdu); concurrency grubuna event adı eklendi
     (grup tek bir pending çalıştırma tutar ve yenisi eskisini iptal eder; gece koşusu
     kuyruktaki bir push'u ve onun deploy'unu iptal edebilirdi).
  5. kubeconform v0.8.0 + `sha256sum -c` + `curl -f`. Yerel (linux/amd64): doğru hash
     `OK`, 33/33 kaynak geçerli; yanlış hash exit 1 ve kubeconform hiç çalışmıyor.
  6. gitleaks v8.30.1 (digest ile), tam geçmiş (`fetch-depth: 0`), `--redact`. Yerel: 109
     commit (85'i diff'li) temiz; sahte `ghp_` token'lı deneme reposu `github-pat`, exit 1.
     "dubious ownership" tahminim yanlıştı: imaj kendisi `safe.directory '*'` ayarlıyor.
  7. 8 action, 19 `uses:` satırı commit SHA'sıyla (`# vX.Y.Z` yorumuyla). Her SHA hem tam
     sürüm tag'inin hem o ana kadar kullanılan major tag'in commit'i: çalışan kod
     değişmedi. Dockerfile'daki iki taban imaj index digest'iyle; amd64 + arm64 build
     başarılı.
  8. `.github/dependabot.yml`: github-actions, gomod (otel modülleri tek grup), docker;
     haftalık. Şema doğrulaması geçti (`interval: hourly` reddedildi). Lint job'u
     Dockerfile'daki golang sürümünü `go.mod`'daki toolchain'le karşılaştırıyor (Dependabot
     ikisini ayrı güncelliyor); sadece Dockerfile'ı 1.26.10 olan kopyada exit 1.
  9. GitHub ayarları (kullanıcı elle yaptı, 2026-10-17): Dependabot alerts ve security
     updates açık. API: `vulnerability-alerts` 204, `dependabot_security_updates: enabled`;
     dependency graph 138 paket okuyor (`x/net v0.60.0`, `otel v1.45.0`); açık alert 0.
     **Plan düzeltmesi (madde 8):** secret scanning ve push protection zaten açıktı.
  - Not (bilgi): Trivy'nin checks bundle'ındaki bazı kurallar Trivy ≥ 0.72 istiyor; action
    0.70 kurduğu için bunlar atlanıyor (uyarı logda).
- **Sonuç (2026-10-17), PR #24'ün CI'ı (run 38987122485, hepsi yeşil):**
  - Zaman çizelgesi: `scan-deps`, `govulncheck`, `gitleaks` +0–1 sn'de başlayıp +12–20 sn'de
    bitti (önce: `security` +250 sn'de başlıyordu). Tarama sonucu ~280 sn yerine ~20 sn'de
    geliyor. Toplam süre 280 sn → 269 sn; kritik yol artık `test` (175 sn) → `docker`.
    Q6'daki ≈252 sn tahminim tutmadı: `docker` job'ları imaj taraması yüzünden 70–86 sn'den
    76–89 sn'ye çıktı.
  - `scan-deps`: "Misconfiguration scanning is enabled"; 10 dosyada 33 HIGH (KSV-0118 ×22,
    KSV-0014 ×11), step yeşil. vuln ve secret tarayıcıları "enabled" yazdı.
  - 7 `docker` job'unun 7'sinde `Trivy image scan`: `alpine 3.24.2` 0, `gobinary` 0;
    "not on the EOL list" uyarısı. `TRIVY_EXIT_ON_EOL: 1` action'ın bütün iç step'lerinin
    env'inde görünüyor. EOL'de gerçekten exit 1 verdiği CI'da değil, yerelde aynı Trivy
    sürümüyle (0.70.0) ve aynı env ile gösterildi.
  - `lint`: "go.mod toolchain: go1.26.9, Dockerfile build stage: golang:1.26.9".
  - `govulncheck`: `go1.26.9`, "No vulnerabilities found". `gitleaks`: 90 commit, "no leaks
    found". `deploy`: skipped.
  - Merge sonrası (2026-10-17, PR #24 → `f3f4ca7`): `main` push run'ında `deploy` çalıştı
    (`kubeconform.tar.gz: OK`, 33/33). Dependabot ilk dakikada 8 PR açtı (#25–#32).
    Dependabot `--platform`'lu FROM satırını tag ve digest birlikte güncelledi; action'larda
    SHA ile `# vX.Y.Z` yorumu birlikte değişti. #29 (`golang` 1.26.9 → 1.27.2, sadece
    Dockerfile) toolchain kontrolünde kırmızı: `go1.26.9` ≠ `golang:1.27.2`. Kontrol
    tasarlandığı işi yaptı.
  - Takip (`chore/dependabot-groups`, kararlar 2026-10-17): golang için minor/major
    güncellemeler `ignore` (A4 kararı Go 1.26; yamalar gelmeye devam ediyor), #29
    kapatılacak; actions ve gomod için minor+patch tek PR'da, major'lar ayrı PR olarak
    kalıyor. Son ekli docker tag'inde (`1.26.9-alpine3.24`) semver türlerinin nasıl
    uygulandığı belgede yok; bir sonraki Dependabot çalıştırmasında doğrulanacak.
  - Major action güncellemeleri (#25–#28, #30): release notlarında ana değişiklik Node 24.
    Bizim kullandığımız input ve env'lerin hiçbiri kaldırılmamış (kod okumasından).
    Mevcut sürümler Node 20'yi hedefliyor ve runner onları Node 24'te çalışmaya zorluyor
    (her job'da `Node.js 20 is deprecated` uyarısı, run 38987122485).
  - **Düzeltme (2026-10-17), Bulgu #61:** Taramalar job olarak kırmızıya dönüyor ama merge'ü
    engellemiyordu. "Protect Main Branch" ruleset'i sadece `lint`, `test` ve `integration`'ı
    zorunlu tutuyor (`strict` açık). `ci/required-check-gate`: `deploy` hariç bütün
    job'ları bekleyen `ci-ok` job'u eklendi, `deploy` artık `ci-ok`'a bağlı. Job
    `if: always()` ile çalışıyor ve bir job bile `success` değilse kendisi kırmızı oluyor.
    GitHub belgesine göre atlanan bir job zorunlu check'e "Success" raporluyor; düz
    `needs` kullanılsaydı kırmızı bir tarama gate'i atlatıp merge'e izin verirdi. Betik
    yerelde dört girdiyle denendi: hepsi success → 0; failure, skipped, cancelled → 1.
    Ruleset'te zorunlu check'i `ci-ok` yapmak kullanıcı tarafından yapılacak.
  - Strict politikanın yan etkisi: her merge diğer PR'ları "BEHIND" yapıyor. Dependabot
    PR'ında "Update branch" bir merge commit ekliyor, Dependabot da o PR'ı bir daha rebase
    etmiyor (belge: "will stop rebasing a pull request once extra commits have been
    pushed"). Doğrusu `@dependabot rebase` yorumu. #26'da "Update branch" kullanıldı;
    PR hemen merge edildiği için bir etkisi olmadı. #28'deki çakışmayı Dependabot bir
    dakika içinde kendisi rebase ederek çözdü.
  - **Kapanış (2026-10-18):** Ruleset'te zorunlu check artık sadece `ci-ok` (GitHub
    Actions, `integration_id` 15368), strict açık; API ile doğrulandı. Dependabot'un ilk
    dalgası kapandı: #25–#28, #30, #31, #39, #40 merge edildi; #29 kapatıldı. #32–#35 grup
    PR'ı #37'de birleşti. #31 merge edilince Dependabot #37'yi "updatable in another way"
    diyerek kapattı ve yerine #40'ı (17 güncelleme, otel satırı yok) açtı; otel'in hepsi
    v1.47.0, contrib v0.72.0. #37'nin önce merge edilmesi otel API'sini 1.47'ye çıkarıp
    sdk'yı 1.45'te bırakacaktı (`go mod graph`: `redisotel v9.23.0`, `otelgorm`/`otelsql
    v0.4.0` otel 1.47 istiyor). `actions/setup-go` v7 (#39) Temmuz'da çıkmıştı; ilk
    çalıştırmada github-actions için 5 PR açılıp varsayılan açık PR sınırına
    takıldığı için sonradan geldi. v7 hâlâ `toolchain` satırını okuyor (kaynakta ve CI
    logunda `go1.26.9`). Bütün action'lar Node 24'te; `Node.js 20 is deprecated` uyarısı
    kalmadı.
  - **Gate kanıtı (PR #41, kapatıldı):** Final imaj bilerek `alpine:3.20`'ye çekildi.
    `lint`, `test`, `integration` (eski zorunlu üçlü) yeşil; imaj taraması `This OS
    version is no longer supported by the distribution` + exit 1 ile docker job'larını
    kırmızıya çevirdi (EOL kontrolünün CI'daki kanıtı); `ci-ok` `failure docker`; PR
    `draft=false`, `MERGEABLE`, `BLOCKED`. Eski kuralla bu PR merge edilebilirdi. İlk
    deneme (sadece golang sürümü) `lint`'i bozduğu için `ci-ok`'un farkını
    göstermiyordu; deney değiştirildi.
  - Yeni Bulgu #62: aynı çalıştırmada 7 docker job'unun 2'si EOL'den değil matrix'in
    `fail-fast` varsayılanıyla iptal edildi (`The operation was canceled`).

### A6. Container registry: imajları GHCR'a gönder

- [ ] **Ne:** `main`'e push'ta imajları `ghcr.io/alpnuhoglu/gamemesh-<servis>:<git-sha>`
  olarak multi-arch (`linux/amd64,linux/arm64`) build edip push etmek. PR'larda sadece build
  yapılıyor, push yok.
- **Neden:** Şu anki manifest'ler imajın node'da zaten var olduğunu varsayıyor
  (`IfNotPresent` + Minikube'ün docker daemon'u). VPS'teki k3s'in imajı bir yerden **çekmesi**
  gerekiyor. GHCR, GitHub reposuyla aynı yetkiyi kullanır ve `GITHUB_TOKEN` ile ekstra
  secret gerekmez; public repo için ücretsizdir.
- **Neden `:latest` değil de `:<sha>`:** `latest` değişebilen (mutable) bir tag'dir. Hangi
  kodun çalıştığını bilemezsin, rollback yapamazsın, ve `IfNotPresent` politikası yüzünden
  node eski `latest`'i kullanmaya devam edebilir. SHA tag'i değişmez (immutable): her deploy
  tek bir commit'e karşılık gelir ve rollback "bir önceki SHA'yı deploy et" kadar basittir.
- **Nasıl:** `docker/login-action` + `docker/metadata-action` + `build-push-action` ile
  `platforms:` ve `push: ${{ github.ref == 'refs/heads/main' }}`. Workflow'a
  `permissions: packages: write` ekle. İlk push sonrası GHCR'da paketleri **public** yap,
  ya da cluster'a `imagePullSecret` ver.
- **Öğrenilecek:** OCI registry, image manifest list (multi-arch), tag ile digest farkı,
  GitHub token izinleri (least privilege).
- **Bitti sayılır:** Mac'ten `docker pull ghcr.io/.../gamemesh-gateway:<sha>` çalışıyor ve
  `docker manifest inspect` iki platformu gösteriyor.
- **İncelemeden eklenenler (M7):** `packages: write` yetkisi workflow seviyesinde değil,
  **sadece push yapan job'da** verilmeli (A5'teki `permissions: contents: read` ile
  birlikte).

### A7. K8s manifest'lerindeki eksikleri tamamla

- [ ] **Ne:**
  1. **NATS** StatefulSet'i: JetStream açık (`-js -sd /data`), `volumeClaimTemplates` ile
     kalıcı disk, `:8222/healthz` probe'ları, Service.
  2. ConfigMap'e `EVENT_BUS: nats` ve `NATS_URL: nats://nats:4222`.
  3. ConfigMap'e `OTEL_ENABLED: "false"`. Faz 1'de collector kurmuyoruz; tracing Faz 2+ işi.
  4. **Redis:** PVC + `--appendonly yes` (AOF), liveness probe ve `Recreate` stratejisi.
  5. Bütün uygulama Deployment'larında: `livenessProbe → /livez`,
     `readinessProbe → /readyz`, gerekirse `startupProbe`.
  6. `securityContext`: `runAsNonRoot: true`, `readOnlyRootFilesystem: true`,
     `allowPrivilegeEscalation: false`, `capabilities: drop: [ALL]`.
- **Neden:**
  - NATS olmadan outbox-relay `log.Fatal` ile çöküp CrashLoopBackOff'ta kalır. `EVENT_BUS`
    tanımsız olduğu için de k8s, compose'tan **farklı** bir event taşıma katmanı (Redis
    Pub/Sub) kullanıyor. Yerelde test ettiğin sistemle canlıda koşan sistem aynı olmalı.
  - NATS ve Redis'in **StatefulSet/PVC** gerektirmesinin nedeni şu: kalıcı veri tutan bir
    pod yeniden başladığında aynı diske bağlanmalı. Deployment pod'ları birbirinin yerine
    geçebilir (cattle), StatefulSet pod'ları ise kimlik ve diskiyle bir bütündür.
  - Redis AOF olmadan bir pod restart'ı leaderboard'u ve tüm oturumları siler.
  - `securityContext` ile container ele geçirilse bile saldırganın yapabilecekleri
    sınırlanır (defense in depth). Statik Go binary'si bu kısıtlarla zaten sorunsuz çalışır.
- **Öğrenilecek:** Deployment ile StatefulSet farkı, PVC/StorageClass, k3s'in
  `local-path` provisioner'ı, Pod Security Standards.
- **Bitti sayılır:** `kubeconform -strict` geçiyor. A10'daki yerel k3s üzerinde bütün
  pod'lar `Running 1/1`, restart sayısı 0.
- **İncelemeden eklenenler:**
  1. **Websocket NATS consumer'ını replica başına ayırmak (M5, yeni bulgu):**
     [pkg/events/nats.go:246-256](../pkg/events/nats.go#L246-L256) bütün websocket
     replica'larına aynı durable consumer adını veriyor (`websocket-<stream>`). JetStream'de
     aynı durable'a bağlanan aboneler mesajları **paylaşır** (work queue): her event
     sadece bir replica'ya gider ve başka bir replica'ya bağlı oyuncu `MatchFound`'u
     kaçırır. Compose'da tek replica, k8s'te Redis Pub/Sub (broadcast) kullanıldığı için
     şimdiye kadar görünmedi; madde 2 (EVENT_BUS=nats) bu hatayı ortaya çıkaracak. Çözüm:
     websocket için pod başına consumer (pod adını içeren bir isim ya da ephemeral
     consumer) ve `DeliverPolicy: DeliverNew` (şu anki `DeliverAll` ile her yeni pod
     stream geçmişini istemcilere tekrar oynatır). Diğer servisler için paylaşılan durable
     doğru davranış. *A3b'den ek:* websocket artık PLAYER stream'ini de dinliyor
     (`SessionRevoked`); aynı paylaşım sorunu logout kapatmalarını da bölüyor.
  2. **Matchmaking için `strategy: Recreate` (M5, yeni bulgu):**
     [12-matchmaking.yaml](../deployments/k8s/12-matchmaking.yaml) "single replica by
     design" diyor, ama varsayılan RollingUpdate (`maxSurge` 1'e yuvarlanır) deploy
     sırasında birkaç saniyeliğine **iki** eşleştirme döngüsü çalıştırıyor. Kuyruk Redis'te
     durduğu için `Recreate`'in bedeli sadece birkaç saniyelik bir eşleşme gecikmesi.
     Uzun vadeli alternatif: yorumda önerilen Redis `SET NX EX` kilidi.
  3. **`preStop` ve `terminationGracePeriodSeconds` (M4):** Bütün uygulama
     Deployment'larına `lifecycle.preStop: sleep 5`. Bütçe:
     `preStop (5) + shutdown timeout (15) = 20 < terminationGracePeriodSeconds (30)`.
     Gerekçe: SIGTERM ile endpoint güncellemesi paralel ilerler. Laboratuvar deneyinde
     Shutdown başladıktan 100 ms sonra gelen bağlantı `connection refused` aldı;
     ingress'te bu 502 demek. `sleep` Alpine'da var; distroless'a geçilirse k8s'in
     yerleşik `sleep` eylemi kullanılmalı.
  3a. **NATS için lame duck modu (A3a'dan):** StatefulSet'e
     `lifecycle.preStop: nats-server --signal ldm` ile istemcilerin bağlantıyı kapanmadan
     önce başka bir sunucuya taşımasına izin vermek (tek node'da etkisi sınırlı).
  4. **Probe parametreleri (M6):** Şu an sadece `initialDelaySeconds` ve `periodSeconds`
     tanımlı; `timeoutSeconds` 1 sn ve `failureThreshold` 3 varsayılanda. Hedef:
     - `startupProbe` (`/livez`, `periodSeconds: 2`, `failureThreshold: 30`). Uzun bir
       migration'da liveness'ın t≈30 sn'de container'ı öldürüp sonsuz döngü yaratmasını
       önler.
     - Readiness: `timeoutSeconds: 2`, `failureThreshold: 2` (hızlı ve hassas).
     - Liveness: `timeoutSeconds: 3`, `failureThreshold: 3` (yavaş ve toleranslı). CPU
       limiti 500m iken 1 sn'lik timeout, yük altında CFS throttling yüzünden probe'ları
       düşürüp bir ölüm sarmalına yol açabilir.
     - İsteğe bağlı: `minReadySeconds`.
  5. **Postgres (M5):** Readiness probe'undaki `pg_isready -U gamemesh` kullanıcı adını
     sabit yazıyor; `sh -c 'pg_isready -U "$POSTGRES_USER"'` ile Secret'tan okunmalı.
     Liveness probe eklenmeli. Gömülü init SQL ConfigMap'i (`postgres-init`, sadece 2
     tablo) kaldırılmalı; şemanın dördüncü kopyası (bkz. Faz 1 sonrası, "tek şema kaynağı").
  6. **Redis (M2/M5 düzeltmesi):** Compose'da Redis imajının `VOLUME /data` tanımı bir
     anonim volume oluşturduğu için veri `stop/start` ve container yeniden oluşturulmasında
     korunuyor, `down`+`up` ile kayboluyor. Kubernetes bu tanımı **yok sayar**; PVC
     olmadan her pod restart'ında veri kaybolur. Madde 4 bu yüzden zorunlu.

### A8. Kustomize ile base / overlay yapısı

- [ ] **Ne:** Düz `deployments/k8s/` klasörünü şuna dönüştürmek:
  ```
  deployments/k8s/
    base/                 # ortamdan bağımsız: Deployment, Service, StatefulSet, ConfigMap
      kustomization.yaml
    overlays/
      local/              # k3d: yerel imajlar, dev secret'ları (secretGenerator), host: gamemesh.localhost
      prod/               # VPS: GHCR imajları, replicas: 1, ingress host + TLS, kaynak limitleri
  ```
- **Neden:** Local ile prod arasında **sadece farklı olması gereken şeyler** farklı olmalı:
  imaj adresi, replica sayısı, domain, secret'lar. Kopyala-yapıştır iki klasör zamanla
  birbirinden uzaklaşır (drift). Kustomize, `kubectl`'in içinde gömülü gelir
  (`kubectl apply -k`); ekstra araç ve şablon dili (Helm) gerektirmez.
- **Prod overlay'de replica azaltma:** Şu an toplam ~14 uygulama pod'u var. Tek node'lu bir
  VPS'te replica'nın yüksek erişilebilirliğe katkısı yoktur (node düşerse hepsi düşer), ama
  RAM tüketir. Prod'da her servise 1 replica veriyoruz.
- **Öğrenilecek:** Kustomize `resources`, `patches`, `images` (imaj tag'ini tek satırla
  değiştirme), `configMapGenerator`/`secretGenerator` ve hash suffix'in pod'ları otomatik
  restart ettirmesi.
- **Bitti sayılır:** `kubectl kustomize deployments/k8s/overlays/prod` ve `.../local`
  hatasız render ediliyor ve çıktı `kubeconform`'dan geçiyor.
- **İncelemeden eklenenler:**
  1. **PodDisruptionBudget (M5, yeni bulgu):** outbox-relay'in PDB'si `minAvailable: 2`.
     Prod'da 1 replica ile bu koşul hiçbir zaman sağlanamaz ve `kubectl drain` (k3s
     güncellemesi, node bakımı) **sonsuza kadar bekler.** Prod overlay'de PDB kaldırılmalı
     ya da `maxUnavailable: 1` yapılmalı.
  2. **Toplam kaynak talebi (M5):** Mevcut manifest'lerde toplam requests
     **cpu=1800m, memory=1792Mi** (toplam memory limit 3584Mi). 2 vCPU'lu bir node'da
     k3s'in kendi pod'larıyla birlikte bu sığmaz ve pod'lar `Pending` kalır. Prod
     overlay'i render edip toplam request'i hesaplamak ve şunu doğrulamak gerekiyor:
     `toplam requests + k3s sistem pod'ları + rolling update'teki 1 fazla pod < node'un
     ayrılabilir kapasitesi`.
  3. **ConfigMap için `configMapGenerator` kullanmak (M5):** ConfigMap içeriği değişince
     pod'lar kendiliğinden yeniden başlamıyor (env değişkenleri process başlarken
     kopyalanır; pod şablonu sadece ConfigMap'in **adını** taşır). Hash'li ad otomatik
     rollout'u ve config'in de geri alınabilmesini sağlar.
  4. **Ingress'i prod'a uyarlamak (M5):** `ingressClassName: traefik` (k3s'te yanlış sınıf
     adı hata vermez, Ingress sessizce yok sayılır), gerçek host, nginx annotation'larının
     kaldırılması. TLS için bkz. C4.
  5. Numaralı dosya sırası (`00-`, `01-`...) yerine Kustomize'ın kaynak türüne göre
     sıralaması (M5).
  6. *(Küçük)* Önerilen etiketler (`app.kubernetes.io/name`, `.../part-of`) (M5).
  7. **Gateway için `TRUSTED_PROXIES` (Bulgu #52):** Prod overlay'inde gateway'e
     `TRUSTED_PROXIES=10.42.0.0/16` (k3s'in varsayılan pod CIDR'ı; Traefik pod'ları
     buradan gelir). Verilmezse gateway Traefik'in IP'sini istemci sanar ve **bütün
     kullanıcılar tek bir rate-limit bucket'ını paylaşır**; tek bir istemci herkesi 429'a
     sokar. Diğer servislerde boş kalır (karar, 2026-10-13: gateway dışında hiçbir servis
     proxy'ye güvenmez). Downstream loglarında `client_ip` gateway'in IP'sidir; gerçek IP
     aynı `request_id` ile gateway logunda bulunur.

### A9. Secret'ları repodan çıkar

- [ ] **Ne:** `01-secrets.yaml`'ı base'den kaldırmak.
  - **local overlay:** dev değerleri `secretGenerator` ile, gitignore'daki bir
    `.env.local`'dan okunuyor.
  - **prod:** Secret repoda **hiç yer almıyor**. CI, GitHub Environment secret'larından
    `kubectl create secret ... --dry-run=client -o yaml | kubectl apply -f -` ile oluşturuyor.
- **Neden:** Repo public. Commit edilmiş bir JWT secret'ı ile herkes geçerli token
  üretebilir ve istediği oyuncu gibi davranabilir. Git geçmişi kalıcıdır; silmek yetmez,
  **değeri değiştirmek** gerekir. Prod değerlerini ilk günden repo dışında üretiyoruz.
- **Öğrenilecek:** K8s Secret'ın şifreleme değil sadece base64 olduğu, secret yönetiminin
  seviyeleri (env → GitHub Secrets → SOPS/Sealed Secrets → External Secrets + Vault).
  Faz 1 için GitHub Secrets yeterli.
- **Bitti sayılır:** `git grep -i jwt_secret deployments/` sonucunda hiçbir değer yok.
- **İncelemeden eklenenler:**
  1. **Production'da güvensiz secret ile açılmayı engellemek, fail fast (M8, yeni bulgu):**
     [pkg/config/config.go:143](../pkg/config/config.go#L143) `JWT_SECRET` tanımlı
     değilse `insecure-dev-secret-do-not-use-in-prod` kullanıyor ve hiçbir yerde kontrol
     yok. k8s ConfigMap'inde `APP_ENV: production` iken Secret'ta
     `minikube-dev-secret-change-me` var. `kubectl apply -f deployments/k8s/` VPS'e
     çalıştırılırsa production, GitHub'da herkesin okuyabildiği bir secret ile sessizce
     açılır. `Load()` içine: `Env == "production"` iken `JWT_SECRET` 32 karakterden kısa
     ya da bilinen bir varsayılan değerse `log.Fatal`. Aynı kontrol `POSTGRES_PASSWORD`
     için de yapılmalı. Hatalı yapılandırma böylece crash'e dönüşür ve readiness sayesinde
     rollout takılır, eski sürüm çalışmaya devam eder.
  2. **Otomatik secret taraması (M8):** gitleaks (pre-commit hook ve CI); GitHub secret
     scanning + push protection. Geçmiş taraması (2026-10-13): dev değerleri bilerek 1–2
     commit'te var; private key, GitHub token ya da AWS anahtarı deseni **0**.
  3. **Prod secret'ları:** `openssl rand -hex 32` ile üretilecek. Grafana admin şifresi
     manifest'te düz bir env değişkeni; monitoring prod'a taşındığında Secret'a alınmalı.
  4. **k8s'te Redis'e şifre (Bulgu #56):** Manifest'lerde `requirepass` ya da
     `REDIS_PASSWORD` yok ve Faz 1'de NetworkPolicy de yok; cluster'daki herhangi bir
     pod (ele geçirilmiş biri de) Redis'e tam erişir. Compose'daki gibi
     (`redis-server --requirepass`, A2a) şifre prod Secret'ından gelmeli; servisler
     `REDIS_PASSWORD`'ü zaten okuyor.

### A10. Prova: yerel k3s (k3d) üzerinde prod'a en yakın deploy

- [ ] **Ne:** `brew install k3d` ile Docker içinde gerçek bir **k3s** cluster'ı kurmak
  (VPS'teki k3s ile aynı dağıtım, aynı Traefik ingress, aynı `local-path` storage). Local
  overlay'i bunun üzerine deploy etmek.
- **Neden:** Minikube nginx ingress kullanıyor, VPS'te Traefik olacak. Annotation'lar
  farklı (WebSocket timeout'ları gibi). Bunu VPS'te öğrenmek yerine burada öğreniyoruz.
  Sunucuya çıkmadan önceki son kapı bu.
- **Nasıl:**
  ```bash
  k3d cluster create gamemesh -p "8080:80@loadbalancer" --agents 0
  # imajları cluster'a yükle (registry'siz):
  k3d image import gamemesh/gateway:dev ... -c gamemesh
  kubectl apply -k deployments/k8s/overlays/local
  kubectl -n gamemesh get pods -w
  curl -H "Host: api.gamemesh.localhost" localhost:8080/health
  ```
- **Öğrenilecek:** k3s bileşenleri (containerd, Traefik, ServiceLB/klipper, local-path,
  CoreDNS), Ingress → Service → Pod trafik yolu, `kubectl describe` / `logs` / `events` ile
  hata ayıklama.
- **Bitti sayılır:** `/health` Traefik üzerinden 200 dönüyor, WebSocket bağlantısı kuruluyor,
  Redis pod'u silinip yeniden oluşturulunca leaderboard verisi duruyor (PVC'nin çalıştığının
  kanıtı).
- **İncelemeden eklenenler, k3d'de ayrıca doğrulanacaklar** (`kubeconform` bunların
  hiçbirini yakalayamaz; şemaya uygun olmak çalışıyor olmak demek değil, M5/M7):
  1. **Websocket fan-out:** Local overlay'de websocket 2 replica. İki farklı pod'a bağlı
     iki istemcinin ikisi de kendi `MatchFound` event'ini alıyor (A7 ek madde 1).
  2. **Matchmaking deploy sırasında tek kopya:** `kubectl rollout restart` sırasında
     aynı anda iki matchmaking pod'u çalışmıyor (A7 ek madde 2).
  3. **Kapanış:** Rolling update sırasında sürekli istek atan bir istemcide 502 yok
     (preStop); WS istemcisi 1001 alıyor (A3a).
  4. **Probe'lar:** Redis durdurulunca leaderboard pod'ları `READY 0/1` oluyor ama
     restart edilmiyor; gateway `READY 1/1` kalıyor ve leaderboard dışındaki route'lar
     çalışıyor.
  5. **ConfigMap değişikliği:** `configMapGenerator`'daki bir değeri değiştirince
     otomatik rollout başlıyor; `rollout undo` eski config'i de geri getiriyor.
  6. **Hatalı deploy:** Bilerek yanlış bir Redis adresiyle deploy edildiğinde rollout
     takılıyor ve eski pod'lar trafik almaya devam ediyor.
  7. **Drain:** `kubectl drain` PDB yüzünden takılmıyor (A8 ek madde 1).
  8. **WS oturum güvenliği:** Logout yapılmış token ile `/ws` 401 alıyor (A3b).
  9. **Gerçek istemci IP'si gateway'e ulaşıyor (Bulgu #52):** İki farklı kaynaktan gelen
     istekler gateway logunda farklı `client_ip` ile görünmeli ve ayrı rate-limit
     bucket'larına düşmeli. Bilgi, test edilmedi: ServiceLB ve Traefik Service'inin
     varsayılan `externalTrafficPolicy: Cluster` ayarı paketleri SNAT'layabilir; o zaman
     Traefik herkesi aynı iç IP olarak görür. Çözüm adayı, `HelmChartConfig` ile
     `externalTrafficPolicy: Local`. k3d'de node'un önünde k3d'nin kendi proxy'si de
     olduğu için kesin sonuç B4'te, VPS üzerinde gerçek bir dış istemciyle alınmalı.

---

## Bölüm B — Sunucu

### B1. VPS seçimi

- [ ] **Ne:** Sağlayıcı, boyut, mimari ve lokasyon seçmek.
- **Boyutlandırma hesabı:** Toplam ihtiyaç kabaca k3s control-plane (~500–700 MB) +
  Postgres (256 MB) + Redis (128 MB) + NATS (~64 MB) + 7 Go servisi × ~30–60 MB + Traefik +
  cert-manager (~150 MB). Toplam ~1,8–2,2 GB. Rahat çalışmak, build/pull sırasındaki ani
  artışlar ve ileride Prometheus/Grafana için **en az 4 GB RAM, 2 vCPU, 40 GB disk**.
- **Öneri:** Hetzner Cloud **CX22** (2 vCPU / 4 GB, x86) ya da **CAX11/CAX21** (ARM, daha
  ucuz; A6'daki multi-arch build sayesinde ikisi de çalışır). Lokasyon: Türkiye'ye en yakın
  Nuremberg/Falkenstein/Helsinki. OS: **Ubuntu 24.04 LTS**.
- **Öğrenilecek:** Kapasite planlama, request/limit ile gerçek tüketim farkı.
- **Bitti sayılır:** SSH anahtarınla `ssh root@<ip>` bağlanıyor.
- **İncelemeden eklenenler:**
  1. **Ölçülmüş tüketim (M2, compose, boşta):** Go servisleri ~33–38 MiB/adet
     (7 servis ≈ 250 MB), Postgres 65, Redis 19, NATS 22 MiB. Gözlemlenebilirlik araçları
     ≈ **545 MB** (Grafana 217, OTel Collector 187, Prometheus 95, Jaeger 46); bu, bütün
     uygulama servislerinin toplamının iki katından fazla. Faz 1'de prod'a taşınmamasının
     nedeni bu.
  2. **Scheduler request'lere bakar, gerçek tüketime değil (M5):** Boyutlandırma A8'deki
     request toplamına göre yapılmalı.
  3. **Rolling update payı (M4):** Tek replica ile kesintisiz deploy, deploy sırasında
     geçici olarak bir pod fazlası (`maxSurge: 1`) için yer gerektirir.

### B2. Sunucu sertleştirme

- [ ] **Ne:**
  1. Root olmayan, sudo yetkili bir kullanıcı. SSH: `PasswordAuthentication no`,
     `PermitRootLogin no`.
  2. **Hetzner Cloud Firewall** (sunucunun dışında, ağ seviyesinde): gelen trafikte
     22 (sadece senin IP'n, mümkünse), 80, 443. Kubernetes API portu **6443 sadece senin
     IP'ne** açık, ya da hiç açılmaz ve SSH tüneliyle erişilir.
  3. `unattended-upgrades` ile otomatik güvenlik güncellemeleri.
  4. Swap kapalı veya küçük. Saat senkronizasyonu (`timedatectl`).
- **Neden:** Public IP'li bir sunucu dakikalar içinde SSH brute-force botlarının hedefi olur.
  6443 açık kalırsa, kimlik doğrulamalı olsa bile cluster'ın API'si internete açılmış olur.
  Firewall'ı sağlayıcı seviyesinde kurmak önemli; k3s iptables kurallarını kendisi yönettiği
  için `ufw` ile çakışabilir.
- **Öğrenilecek:** Attack surface, network firewall ile host firewall farkı, SSH anahtar
  kimlik doğrulaması.
- **Bitti sayılır:** Başka bir ağdan `nc -zv <ip> 6443` zaman aşımına uğruyor, şifreyle SSH
  reddediliyor.
- **İncelemeden eklenenler (M2):** Linux'ta Docker, yayınlanan portlar için iptables'a
  kendi `DOCKER` zincirini yazar ve bu kurallar `ufw`'den **önce** değerlendirilir. Yani
  `ufw deny 5432` olsa bile Docker'ın yayınladığı port internete açık kalır. k3s de
  iptables'ı kendisi yönettiği için host firewall'una güvenilmemeli. Sağlayıcı seviyesindeki
  firewall (Hetzner Cloud Firewall) bu yüzden zorunlu. Sunucuda Docker ile port
  yayınlanmamalı.

### B3. k3s kurulumu

- [ ] **Ne:**
  ```bash
  curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=stable \
    sh -s - --tls-san <public-ip> --write-kubeconfig-mode 600
  sudo k3s kubectl get nodes
  ```
  `/etc/rancher/k3s/k3s.yaml` dosyasını Mac'ine kopyala, `server:` adresini düzelt, ayrı
  bir context olarak ekle (`kubectl config use-context gamemesh-prod`).
- **Neden:** k3s, tek binary'lik ve düşük kaynaklı, CNCF sertifikalı bir Kubernetes. Tek node
  için kubeadm'den çok daha az iş gerektirir ve Traefik, ServiceLB ile local-path storage
  hazır gelir. `--tls-san` olmadan API sertifikası public IP'yi içermez ve uzaktan
  `kubectl` TLS hatası verir.
- **Öğrenilecek:** systemd servisi olarak k3s, kubeconfig yapısı (cluster/user/context),
  `kubectl` context'leri arasında güvenli geçiş. Yanlış cluster'a `apply` etmemek için
  context adını prompt'ta göstermek iyi bir alışkanlık.
- **Bitti sayılır:** Mac'ten `kubectl --context gamemesh-prod get nodes` → `Ready`.

### B4. İlk deploy (TLS'siz, IP üzerinden)

- [ ] **Ne:** Prod Secret'ını elle oluştur
  (`openssl rand -hex 32` ile JWT, ayrıca rastgele bir DB şifresi), sonra
  `kubectl apply -k deployments/k8s/overlays/prod`.
- **Neden:** DNS ve TLS'i işe karıştırmadan önce uygulamanın sunucuda ayağa kalktığını
  ayrıca doğruluyoruz. Bir sonraki adımda bir şey bozulursa, sorunun DNS/TLS'te olduğunu
  bileceğiz. Her seferinde tek değişken.
- **Bitti sayılır:** `curl -H "Host: api.<domain>" http://<ip>/health` → 200.
- **İncelemeden eklenenler (M5/M8):** **Asla** `kubectl apply -f deployments/k8s/`
  (eski, düz klasör) kullanılmamalı; o yol dev secret'larını ve Minikube ayarlarını
  production'a taşır. A9'daki fail-fast kontrolü bu hatayı yakalayacak son savunma hattı.

---

## Bölüm C — Domain, DNS, TLS

### C1. Domain ve DNS

- [ ] **Ne:** Domain al (Cloudflare Registrar, Porkbun, Namecheap gibi). DNS'te
  `api.<domain>  A  <public-ip>` kaydı, TTL 300.
- **Neden:** Let's Encrypt IP adresine değil, domain'e sertifika verir. Düşük TTL, ilk
  günlerde IP değişirse yayılmanın hızlı olmasını sağlar. Cloudflare kullanıyorsan **ilk
  aşamada proxy kapalı olmalı (gri bulut)**; aksi halde Cloudflare kendi sertifikasını araya
  koyar ve HTTP-01 doğrulaması ile WebSocket davranışı karmaşıklaşır.
- **Öğrenilecek:** A/AAAA/CNAME kayıtları, TTL, recursive resolver ile authoritative
  nameserver farkı, `dig +trace`.
- **Bitti sayılır:** `dig +short api.<domain>` → `<public-ip>`. Adım B4'teki curl artık
  `Host` header'ı olmadan `http://api.<domain>/health` ile çalışıyor.

### C2. cert-manager kurulumu

- [ ] **Ne:** Resmi, sürümü sabitlenmiş manifest ile kurulum:
  `kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/vX.Y.Z/cert-manager.yaml`
- **Neden:** cert-manager, sertifikaları Kubernetes kaynağı (`Certificate`) olarak yönetir:
  ACME challenge'ını çözer, sertifikayı bir Secret'a yazar ve süresi dolmadan (90 günlük
  sertifikayı ~60. günde) **otomatik yeniler**. Elle certbot kullanmak, unutulmuş bir
  yenileme yüzünden kesinti demektir.
- **Öğrenilecek:** CRD (Custom Resource Definition), controller/operator deseni
  (istenen durum ↔ gerçek durum reconcile döngüsü).
- **Bitti sayılır:** `kubectl -n cert-manager get pods` çıktısında 3 pod Running.

### C3. ClusterIssuer: önce staging, sonra prod

- [ ] **Ne:** İki `ClusterIssuer`: `letsencrypt-staging` ve `letsencrypt-prod`. İkisi de
  ACME **HTTP-01** solver'ı ve `ingressClassName: traefik` kullanıyor. İlk denemeyi
  **staging** ile yap.
- **Neden:** Let's Encrypt prod ortamında sıkı rate limit'ler var (aynı domain için haftada
  5 başarısız doğrulama gibi). Yanlış yapılandırmayla birkaç deneme seni bir hafta
  bloklayabilir. Staging'in limiti çok daha geniştir; sertifikası tarayıcıda "güvenilmez"
  görünür ama akışın çalıştığını kanıtlar.
- **HTTP-01 nasıl çalışır:** cert-manager geçici bir pod ve Ingress kurar. Let's Encrypt
  `http://api.<domain>/.well-known/acme-challenge/<token>` adresine istek atar ve doğru
  yanıtı görürse domain'in sende olduğunu kabul eder. Bu yüzden **80 portu açık olmalı**
  (B2) ve DNS doğru olmalı (C1).
- **Öğrenilecek:** ACME protokolü, HTTP-01 ile DNS-01 farkı (wildcard sertifika için
  DNS-01 gerekir).
- **Bitti sayılır:** Staging ile `kubectl get certificate` → `READY=True`. Ardından prod
  issuer'a geçilmiş ve o da `READY=True`.

### C4. Ingress'e TLS ve HTTPS yönlendirmesi

- [ ] **Ne:** Prod overlay'deki Ingress'te:
  - `cert-manager.io/cluster-issuer: letsencrypt-prod` annotation'ı
  - `tls: [{hosts: [api.<domain>], secretName: api-tls}]`
  - Traefik `Middleware` ile HTTP→HTTPS yönlendirmesi (redirectScheme)
  - Path'ler: `/health`, `/api`, `/ws`
  - Nginx'e özel `proxy-read-timeout` annotation'larını kaldır (Traefik'te bir karşılığı
    yok; Traefik WebSocket'i varsayılan olarak destekler).
- **Neden:** TLS olmadan JWT token'ları ve şifreler ağda düz metin olarak gider. Tarayıcılar
  da HTTPS sayfasından `ws://` bağlantısına izin vermez (mixed content); `wss://` gerekir.
- **Öğrenilecek:** TLS termination'ın ingress'te yapılması (küme içi trafik düz HTTP),
  SNI, sertifika zinciri.
- **Bitti sayılır:**
  ```bash
  curl -sSf https://api.<domain>/health            # 200
  curl -sI http://api.<domain>/health              # 301/308 → https
  openssl s_client -connect api.<domain>:443 -servername api.<domain> </dev/null \
    | openssl x509 -noout -issuer -dates            # Let's Encrypt, geçerli
  ```
  **Faz hedefinin ilk yarısı burada tamamlanıyor.**
- **İncelemeden eklenenler:**
  1. **Dışarı sadece `/health`, `/api` ve `/ws` açılır (M3):** `/metrics`, `/livez`,
     `/readyz` ve `/healthz` internete açılmamalı. Metrikler iç yapıyı anlatır (endpoint
     listesi, hata oranları, kuyruk boyutları).
  2. **`/ws` doğrudan websocket servisine gidiyor (M8):** Gateway'in kimlik ve oturum
     kontrolü atlanıyor. A3b tamamlanmadan bu route internete açılmamalı.

---

## Bölüm D — Otomatik deploy (GitHub Actions)

### D1. CI için kısıtlı yetkili cluster erişimi

- [ ] **Ne:** Cluster'da sadece `gamemesh` namespace'ine yetkisi olan bir `ServiceAccount`
  + `Role` + `RoleBinding` oluşturmak. Bu hesaba ait bir token'la kubeconfig üretmek ve
  GitHub'da `production` **Environment**'ına `KUBECONFIG` secret'ı olarak koymak.
- **Neden:** `/etc/rancher/k3s/k3s.yaml` cluster-admin yetkisi taşır. Onu GitHub'a koyarsan,
  bir workflow sızıntısında bütün cluster ele geçer (least privilege ihlali). GitHub
  Environment'ları ek olarak "sadece `main` branch'i deploy edebilir" kuralı ve isteğe bağlı
  manuel onay sağlar.
- **6443 sorunu:** B2'de 6443'ü dışarıya kapattık, oysa GitHub runner'ları değişken IP'lerden
  gelir. Seçenekler:
  - (a) Tailscale GitHub Action ile runner'ı özel ağa almak (önerilen),
  - (b) cluster içinde self-hosted runner,
  - (c) 6443'ü açıp yalnızca token kimlik doğrulamasına güvenmek (en zayıf seçenek).

  Bu karar bu adımda verilecek.
- **Öğrenilecek:** Kubernetes RBAC (Role ile ClusterRole farkı), ServiceAccount token'ları,
  GitHub Environments ve protection rule'ları.
- **Bitti sayılır:** Bu kubeconfig ile `kubectl -n gamemesh get pods` çalışıyor ama
  `kubectl get nodes` **Forbidden** dönüyor.

### D2. Deploy job'ı

- [ ] **Ne:** [ci.yml](../.github/workflows/ci.yml)'deki `echo` tabanlı örnek job'ı gerçek
  bir deploy ile değiştirmek:
  ```yaml
  deploy:
    environment: production
    concurrency: { group: deploy-prod, cancel-in-progress: false }
    steps:
      - kustomize edit set image ...=ghcr.io/...:${{ github.sha }}   # 7 servis
      - kubectl create secret ... --dry-run=client -o yaml | kubectl apply -f -
      - kubectl apply -k deployments/k8s/overlays/prod
      - kubectl -n gamemesh rollout status deploy/<her servis> --timeout=180s
  ```
- **Neden:**
  - `concurrency`: iki ardışık push'un deploy'ları paralel koşup birbirini ezmemeli.
  - `rollout status`: `apply` sadece "isteği kabul ettim" demektir. Pod'ların gerçekten
    ayağa kalkıp readiness'tan geçtiğini (A3'teki `/readyz`!) bu adım bekler. Başarısız
    olursa job kırmızıya döner, sen de haberdar olursun.
  - Rolling update + readiness sayesinde eski pod'lar, yenisi hazır olmadan kapatılmaz.
- **Öğrenilecek:** Rolling update stratejisi (`maxSurge`, `maxUnavailable`), declarative
  apply, idempotency.
- **Bitti sayılır:** README'ye küçük bir değişiklik push'lanınca 10 dakika içinde canlıda
  yeni SHA çalışıyor (`/health` yanıtındaki `version` alanı ile doğrulanıyor).
- **İncelemeden eklenenler (M6/M7):**
  1. `rollout status` yalnızca readiness doğru yazılmışsa anlamlıdır. Şu anki shallow
     `/healthz` ile Redis'e ulaşamayan bir sürüm de "başarılı" sayılır ve trafiğin
     tamamını alır. A3 bu adımın ön koşulu.
  2. Deployment'ın `progressDeadlineSeconds` değeri (varsayılan 600 sn) `rollout status`
     timeout'u ile uyumlu seçilmeli.
  3. Deploy job'ı `cancel-in-progress: false` kullanmalı (A5'teki CI ayarının tersi):
     yarıda kesilmiş bir deploy cluster'ı tutarsız bırakabilir.

### D3. Smoke test ve rollback

- [ ] **Ne:** Deploy'dan sonra
  `curl --retry 10 --retry-all-errors -fsS https://api.<domain>/health` adımı. Başarısız
  olursa `kubectl rollout undo` çalışıyor.
- **Neden:** "Pod hazır" ile "kullanıcı dışarıdan ulaşabiliyor" aynı şey değildir. Arada
  Ingress, TLS ve DNS var. Faz hedefi tam olarak bu dış kontrol olduğu için CI her deploy'da
  onu doğrulamalı.
- **Bitti sayılır:** Bilerek bozuk bir imaj deploy edildiğinde job kırmızıya dönüyor ve
  canlıdaki eski sürüm hizmet vermeye devam ediyor.
- **İncelemeden eklenenler (A1):** Smoke test'i login akışıyla genişletmek istenirse,
  login body'si `email` değil `identifier` alanı bekliyor.

### D4. Dokümantasyon ve runbook

- [ ] **Ne:** [docs/deployment.md](deployment.md)'yi güncellemek:
  - k3d ile yerel çalıştırma
  - VPS kurulumunun tekrar edilebilir adımları
  - Secret rotasyonu
  - Rollback
  - "Sertifika yenilenmedi" ve "Pod CrashLoop'ta" için sorun giderme
- **Neden:** Sunucu bir gün tamamen silinirse bu doküman sıfırdan kurulumun tek kaynağı
  olur. Bunu bir tatbikatla doğrulamak iyi bir alıştırma.
- **Bitti sayılır:** Doküman adım adım izlenerek boş bir sunucuya kurulum yapılabiliyor.
- **İncelemeden eklenenler:**
  1. **Dokümandaki hatalar (M7):** CI bölümü "client-side `kubectl apply --dry-run`"
     diyor; gerçekte kubeconform kullanılıyor. Minikube bölümü "five images" diyor;
     servis sayısı 7.
  2. **Secret sızıntısı runbook'u (M8):** Sıra: (1) önce rotasyon (silmek yetmez),
     (2) etkiyi kapatmak (JWT rotasyonu bütün kullanıcıları çıkış yaptırır; kesintisiz
     rotasyon için JWT başlığında `kid` ile bir süre iki anahtarla doğrulama), (3) loglarda
     şüpheli kullanımı aramak, (4) en son ve isteğe bağlı olarak `git filter-repo` (fork'lar
     ve klonlar değişmez).
  3. **Hata ayıklama komutları (M6):** `get pods` (READY 0/1 ile RESTARTS artışının farkı),
     `describe pod` (probe event'leri, `OOMKilled`), `get endpointslices`,
     `rollout status`. CrashLoopBackOff (container kendisi çıkıyor) ile liveness kaynaklı
     restart arasındaki ayrım. Exit code'lar: 137 = SIGKILL (ya da OOM), 143 = SIGTERM.

---

## Faz 1 sonrasına bilinçli olarak bırakılanlar

Bunlar unutulmadı; Faz 1'i sade tutmak için ertelendi:

- **Postgres yedekleri** (`pg_dump` CronJob + offsite depolama). *Gerçek kullanıcı verisi
  gelmeden önce mutlaka yapılmalı.*
- Prometheus/Grafana/tracing'in prod'a taşınması
- NetworkPolicy (pod'lar arası trafik kısıtlaması). Namespace tek başına bir ağ sınırı
  değil (M5).
- GitOps (Argo CD / Flux): push tabanlı deploy yerine pull tabanlı
- Postgres için CloudNativePG gibi bir operator ya da managed DB
- Çok node'lu cluster ve gerçek yüksek erişilebilirlik

**İncelemeden eklenenler:**

- **Tek şema kaynağı (M2/M5):** Şema şu an dört yerde: `scripts/db/schema.sql` (initdb),
  `migrations/0001–0004` ("source of truth" deniyor ama otomatik uygulanmıyor;
  `schema_migrations` tablosu yok), GORM `AutoMigrate` (player açılışında) ve k8s
  `postgres-init` ConfigMap'i. Tek, sürümlü bir migration aracına geçilmeli. GORM
  `AutoMigrate` sütun silmez ve tip değiştirmez.
- **AutoMigrate yarışı (M5):** Player'ın birden fazla replica'sı aynı anda migration
  çalıştırır. Deploy öncesi bir Kubernetes **Job** ya da `pg_advisory_lock`. Prod'da tek
  replica olduğu için Faz 1'de risk düşük.
- **JWT'yi asimetrik algoritmaya taşımak (M8):** HS256'da secret'ı tutan üç servis
  (gateway, player, websocket) token **üretebilir**. EdDSA/RS256 ile sadece player imzalar,
  diğerleri public key ile doğrular. Gerekirse JWKS endpoint'i. Mevcut
  `WithValidMethods([]string{"HS256"})` (algorithm confusion koruması) korunmalı.
- **Secret'ları dosya olarak mount etmek (M8):** Şu an env değişkenleri `docker inspect`
  ve `docker compose config` çıktılarında, `/proc/1/environ`'da ve pod spec'inde düz metin.
  `_FILE` konvansiyonu (`POSTGRES_PASSWORD_FILE` gibi) ve tmpfs mount.
- **distroless taban imaj (M1):** ~2 MB, shell yok. Compose healthcheck'leri için
  binary'ye `/service healthcheck` alt komutu gerekir; preStop için k8s'in yerleşik
  `sleep` eylemi.
- **Prometheus için `ClusterRole` yerine namespace'e sınırlı `Role`** (least privilege, M5).
- **Gateway, başarılı logout'ta kendi oturum önbelleğindeki `jti`'yi silsin (A3b):** Bugün
  logout'tan sonra 5 sn boyunca gateway'de token hâlâ geçerli (ölçüldü: hemen 200, 6 sn
  sonra 401). Logout isteği zaten gateway'den geçtiği için yanıt başarılıysa yerel girdi
  silinebilir.
- **go-redis 9.23'ün ayrı pipeline havuzu (A5a takibi, #40):** Her `Client` artık
  `PipelinePoolSize` (varsayılan 10) boyutunda ayrı bir pipeline havuzu açıyor (release
  notlarından, kod okuması). WS sweep'inin `ExistsMany` pipeline'ı el sıkışma kontrolleriyle
  aynı havuzu paylaşmıyor; client başına Redis'e en fazla 10 bağlantı daha. Ana havuzun
  varsayılanında (10 × GOMAXPROCS) değişiklik notlarda geçmiyor. Havuz metrikleri bu iki
  havuzu ayrı göstermeli.
- **go-redis havuz metrikleri (A3b):** `PoolStats` (`Timeouts`, `TotalConns`, `Hits`)
  Prometheus'a aktarılsın; havuz tükenmesi prod'da tahminle değil ölçümle görülsün.
- **Gateway'de upstream başına circuit breaker** (M3, Soru 2): Sürekli hata veren bir
  upstream'e bir süre hiç istek göndermeden anında 503 dönmek.
- **CPU limiti stratejisi (M6):** CPU limitleri, CFS throttling ve probe timeout'ları
  arasındaki etkileşim (ölüm sarmalı riski) throughput post-mortem'üyle birlikte
  değerlendirilmeli.
- **CI'a eklenebilecekler (M7):** Uçtan uca (e2e) test, k6 ile performans regresyon testi,
  Dockerfile lint'i (hadolint), `gofmt -l` kontrolü (`internal/player/model.go` `main`'de
  formatsız; golangci-lint'te `gofmt` formatter'ı açık değil, 2026-10-13'te fark edildi).
- **İstemci tarafı (M4):** WS istemcisinde jitter'lı yeniden bağlanma ve bağlandıktan
  sonra durumu yeniden çekme (resync).
- **Matchmaking event'leri için outbox (A3a):** Planlı kapanışta tur artık bitiyor, ama
  SIGKILL ya da OOM'da "oyuncular kuyruktan çıktı, `MatchFound` yayınlanmadı" boşluğu
  kalıyor. Event'i oda kaydıyla aynı atomik adımda yazmak (Redis'te outbox ya da
  istemcinin oda durumunu sorgulaması) bunu kapatır.
- **Outbox tüketicilerinin idempotent olduğunun doğrulanması (M4):** Relay at-least-once
  çalışıyor; kapanış sırasında yayınlanıp işaretlenemeyen satırlar tekrar yayınlanır.
- **Go 1.27'ye geçiş (A5a takibi):** `go.mod` toolchain'i, Dockerfile'daki `golang` tag'i
  ve digest'i aynı PR'da; ardından testler ve govulncheck. Go 1.26'nın desteği Go 1.28 ile
  (beklenen: Şubat 2027) bitiyor, o tarihten önce yapılmalı. Dependabot golang minor'ını
  bilerek önermiyor.
- **Kalan iki taşınabilir referans (A5a):** Dockerfile'daki `# syntax=docker/dockerfile:1`
  satırı bir tag (digest ile sabitlenebilir; Dependabot'un bu satırı güncelleyip
  güncellemediği test edilmedi). Trivy sürümü action'ın varsayılanına bağlı; `version:`
  ile sabitlenip Dependabot dışında elle güncellenmeli.

---

## Bulgu dizini

Mevcut yapının incelenmesinde (M1–M8) bulunan her öneri ve hangi adımda ele alındığı.
"Kanıt" sütunu: **Ölçüldü** = komut/deney çıktısıyla gösterildi · **Kod** = kod veya
manifest okunarak tespit edildi, henüz çalışan bir sistemde test edilmedi.

| # | Bulgu | Kaynak | Kanıt | Adım |
|---|---|---|---|---|
| 1 | `.dockerignore` yok; `.env` ve `.git` build aşamasına giriyor | M1 | Ölçüldü | A2 (çözüldü) |
| 2 | Dockerfile yorumu "~15 MB" diyor; gerçek 40–45 MB | M1 | Ölçüldü | A2a (çözüldü) |
| 3 | Go 1.25 desteklenmiyor; Alpine 3.20 destek süresi doğrulanmalı | M1, M7 | Kod / sürüm | A4 (çözüldü: Go 1.26.9, Alpine 3.24.2; 3.20'nin desteği 2026-04-01'de bitmişti) |
| 4 | Go sürümleri her yerde farklı (1.25 / 1.26 / 1.27) | M7 | Ölçüldü | A4 (çözüldü: `toolchain go1.26.9` tek kaynak) |
| 5 | `USER app` sayısal değil (`runAsNonRoot` doğrulayamaz) | M1 | Kod | A4 (çözüldü: `10001:10001`; k8s'te doğrulama A10) |
| 6 | Portlar `0.0.0.0`'a bağlı; LAN'dan Postgres'e ve şifresiz Redis'e erişilebiliyor | M2 | Ölçüldü | A2a (çözüldü) |
| 7 | Compose'da `restart:` politikası yok | M2 | Ölçüldü | A2a (çözüldü: `unless-stopped`) |
| 8 | `.env` eski (15 anahtar eksik), izinleri 644 | M8 | Ölçüldü | A2a (çözüldü) |
| 9 | Healthcheck "healthy" derken istekler 500 dönüyor (shallow) | M2, M3 | Ölçüldü | A3 (çözüldü, compose dahil) |
| 10 | Liveness ve readiness aynı endpoint'i kullanıyor | M3, M6 | Kod | A3 (çözüldü), k8s probe'ları A7 |
| 11 | Health ve metrics istekleri log, metrik ve trace üretiyor | M3 | Ölçüldü | A3 (çözüldü) |
| 12 | Health'in rate limit muafiyeti satır sırasına bağlı, test yok | M3 | Ölçüldü | A3 (çözüldü) |
| 13 | Arka plan döngüsü ölürse `/healthz` yine "ok" der | M3 | Kod | A3 (çözüldü) |
| 14 | Public `/health` route'u yok; yanıtta sürüm bilgisi yok | M3 | Kod | A3 (çözüldü: route) + A4 (çözüldü: SHA) |
| 15 | WebSocket istemcileri kapanışta 1006 alıyor; close frame boş (1005) | M4 | Ölçüldü | A3a (çözüldü: 1001) |
| 16 | Arka plan işçilerinin bitmesi beklenmiyor | M4 | Kod | A3a (çözüldü) |
| 17 | Shutdown timeout'unda `log.Fatal` `defer`'leri atlıyor | M4 | Kod | A3a (çözüldü) |
| 18 | Matchmaking'in kapanışta yarıda kesilmesinin etkisi doğrulanmadı | M4 | Açık soru | A3a (çözüldü: planlı kapanış; SIGKILL boşluğu Faz 1 sonrası) |
| 19 | **Websocket oturum kontrolü yapmıyor; logout WS'i kapatmıyor** | M8 | Ölçüldü | A3b (çözüldü) |
| 20 | CI'da 7 servisin 5'i build ediliyor | M7 | Kod | A5 (çözüldü: matrix `cmd/`'den) |
| 21 | `build` job'u gereksiz; docker `.tar` artifact'ları kullanılmıyor | M7 | Kod | A5 (çözüldü) |
| 22 | `permissions` ve `concurrency` tanımlı değil | M7 | Kod | A5 (çözüldü) |
| 23 | `security`, gereksiz yere `docker` job'unu bekliyor | M7 | Ölçüldü (süreler) | A5a (çözüldü: taramalar `needs`'siz; +250 sn → +0 sn) |
| 24 | İmaj taraması yok; Alpine OS paketleri taranmıyor | M7 | Kod | A5a (çözüldü: 7 imaja `trivy image` + EOL kontrolü) |
| 25 | govulncheck production'dan farklı bir Go sürümünü denetliyor | M7 | Ölçüldü (CI logu) | A4'te yanlışlıkla çözüldü sayıldı; A5'te çözüldü (`go-version-input: ""`, CI'da 1.26.9) |
| 26 | Güvenlik taraması doküman PR'larını blokluyor (geçmişte 4 kırmızı) | M7 | Ölçüldü (GitHub API) | A5a (gece taraması; doküman PR'ları bilerek muaf tutulmadı) |
| 27 | Action'lar tag ile sabitlenmiş; kubeconform `latest` ve checksum'sız | M7 | Kod | A5a (çözüldü: action SHA, imaj digest, kubeconform checksum, Dependabot) |
| 28 | K8s'te NATS yok; `EVENT_BUS` tanımsız (k8s Redis Pub/Sub kullanıyor) | M5 | Kod | A7 |
| 29 | **Websocket replica'ları NATS consumer'ını paylaşıyor (event'ler bölünüyor)** | M5 | Kod | A7, A10 |
| 30 | **Matchmaking rolling update sırasında iki kopya çalışıyor** | M5 | Kod | A7, A10 |
| 31 | `preStop` ve `terminationGracePeriodSeconds` yok (endpoint yarışı) | M4 | Ölçüldü (lab) | A7 |
| 32 | `startupProbe` yok; probe timeout'ları varsayılan (1 sn) | M6 | Kod | A7 |
| 33 | Redis'in PVC'si yok; k8s imajdaki `VOLUME` tanımını yok sayıyor | M2, M5 | Ölçüldü (compose) | A7 |
| 34 | Postgres probe'unda kullanıcı adı sabit; liveness yok; init SQL ConfigMap'i | M5 | Kod | A7 |
| 35 | **PDB `minAvailable: 2` tek replica ile node drain'i kilitliyor** | M5 | Kod | A8, A10 |
| 36 | Toplam requests 1800m CPU, 2 vCPU'ya sığmıyor | M5 | Ölçüldü (hesap) | A8, B1 |
| 37 | ConfigMap değişikliği pod'ları yeniden başlatmıyor | M5 | Ölçüldü (Docker analog) | A8 |
| 38 | Ingress nginx'e özel; k3s'te sessizce yok sayılır | M5 | Kod | A8, C4 |
| 39 | Secret'lar repoda (`01-secrets.yaml`) | M5, M8 | Kod | A9 |
| 40 | **Uygulama production'da güvensiz secret ile sessizce açılıyor** | M8 | Kod | A9 |
| 41 | Otomatik secret taraması yok | M8 | Kod | A9, A5a (gitleaks; GitHub secret scanning ve push protection zaten açıktı, API ile doğrulandı) |
| 42 | `kubeconform` anlamsal hataları yakalayamıyor | M5, M7 | Kod | A10 |
| 43 | Docker/k3s `ufw`'yi atlıyor | M2 | Bilgi | B2 |
| 44 | `/ws` ingress'te gateway'i atlıyor | M8 | Kod | A3b (çözüldü: websocket artık oturumu kendisi kontrol ediyor); C4'te `/ws` açılabilir |
| 45 | `rollout status` shallow readiness ile anlamsız | M6 | Kod | D2 |
| 46 | `docs/deployment.md` CI ve servis sayısı bilgisi hatalı | M7 | Kod | D4 |
| 47 | Şema dört ayrı yerde; `AutoMigrate` yarışı | M2, M5 | Kod | Faz 1 sonrası |
| 48 | HS256: üç servis token üretebilir | M8 | Kod | Faz 1 sonrası |
| 49 | Secret'lar env çıktılarında düz metin | M8 | Ölçüldü | Faz 1 sonrası |
| 50 | Prometheus `ClusterRole` kullanıyor | M5 | Kod | Faz 1 sonrası |
| 51 | Yeni CVE-2026-78669 (`x/net` v0.58.0, HIGH) bütün PR'larda security job'unu kırıyor; düzeltmesi Go 1.26 istiyor | M7 | Ölçüldü (CI run 38411117929, yerel Trivy) | `fix/deps-x-net` (çözüldü), kalıcı çözüm A5a |
| 52 | **Gateway bütün proxy'lere güveniyor; sahte `X-Forwarded-For` rate limit'i tamamen atlatıyor** | M3 | Ölçüldü (200 istek: 108×200/92×429 → 200×200) | `fix/gateway-trusted-proxies` (çözüldü: sahte XFF ile 200×200 → 119×200/81×429); prod ayarı A8, doğrulama A10/B4 |
| 53 | `ReverseProxy.Director` Go 1.26'da deprecated; `Rewrite`'a geçiş XFF ve `Host` davranışını değiştiriyor | M3 | Ölçüldü (staticcheck SA1019) | `fix/gateway-trusted-proxies` (çözüldü) |
| 54 | GO-2026-6505 (otel otlptrace v1.44.0, erişilebilir); Trivy kırmızıyken govulncheck adımı hiç çalışmadığı için görünmüyordu | M7 | Ölçüldü (CI run 38416862319, yerel govulncheck) | `fix/deps-x-net` (çözüldü), maskeleme A5a (çözüldü: ayrı job'lar) |
| 55 | Presence (8086) ve websocket (8084) compose'da `0.0.0.0`'a yayınlı; presence kimliksiz bir iç API (LAN'dan herhangi bir oyuncunun durumu değiştirilebilir) | M2 | Ölçüldü (token'sız `POST /presence/connect` → 200) | A2a (çözüldü) |
| 56 | k8s'teki Redis şifresiz; NetworkPolicy olmadan cluster'daki her pod erişebiliyor | M5, M8 | Kod | A9 |
| 57 | Postgres DSN'inde `connect_timeout`/`statement_timeout` yok; ağ bölünmesinde sorgular ve açılış asılı kalabilir | M4 | Kod | A3a (çözüldü: 75 sn → 5 sn, açılışta retry) |
| 58 | Docker Desktop kapanırken container'ları "elle durduruldu" işaretliyor; `unless-stopped` stack'i geri getirmiyor (A2a iddiasının düzeltmesi) | M2 | Ölçüldü (`HasBeenManuallyStopped: true`, 14/14) | Karar: `restart: always` (Docker Desktop yeniden açılışında doğrulanacak) |
| 59 | Açık WS bağlantısı JWT'nin süresi dolduktan sonra da açık kalıyor (token sadece el sıkışmada kontrol ediliyordu) | M8 | Kod, test ile doğrulandı | A3b (çözüldü: tarama `1008 "token expired"`) |
| 60 | Trivy'ye `scanners` verilmemiş; step adı "misconfig" dese de manifest'ler hiç taranmıyordu (33 HIGH görünmüyordu) | M7 | Ölçüldü (yerel Trivy 0.70.0) | A5a (misconfig sadece rapor; bloklama A7) |
| 61 | `main` ruleset'inde zorunlu check'ler sadece `lint`, `test`, `integration`; taramalar, gitleaks ve imaj build/taramaları kırmızıyken de merge mümkün (A5a'daki "bloklayan" iddiasının düzeltmesi) | M7 | Ölçüldü (rulesets API) | A5a takibi: `ci-ok` toplayıcı job'u; ruleset değişikliği kullanıcı tarafından |
| 62 | `docker` matrix'i `fail-fast: true` (varsayılan): bir servisin job'u düşünce kardeş job'lar iptal ediliyor, diğer imajların tarama sonucu görünmüyor (#54'ün matrix'teki karşılığı) | M7 | Ölçüldü (PR #41: 5 EOL, 2 `canceled`) | A6 (`fail-fast: false`) |
