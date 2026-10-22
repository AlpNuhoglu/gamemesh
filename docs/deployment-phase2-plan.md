# Deployment Faz 2 — Gerçek kullanıcıya hazırlık

**Hedef:** Sistem, verisini kaybetmeden ve sorun çıktığında **haber vererek** gerçek
kullanıcı taşıyabilir. Faz 1 sistemi canlıya aldı (`docs/deployment-phase1-plan.md`); Faz 2
üç eksiği kapatıyor:

1. **Veri dayanıklılığı:** Bugün verinin tek kopyası sunucunun diskinde. Disk, sunucu ya da
   bir hata giderse veri gider.
2. **Dış izleme:** Bugün sistem çökerse ya da sertifika yenilenmezse bunu ilk öğrenen
   kullanıcı olur (Bulgu #85). Ev ağından yapılan ölçümler de bazen yanıltıcı (Bulgu #83).
3. **Prod'da monitoring:** Prometheus/Grafana sadece compose'da ve yerel k3d'de çalışıyor
   (Bulgu #68); prod'da ne olduğunu sonradan göremiyoruz.

Faz 1'den kalan **tarihli işler** de bu fazın içinde, tarihlerinde yapılıyor (Bölüm H).

## Tamamlanma kriterleri (Faz 2 "bitti" sayılır)

Her kriter bir ölçümle kanıtlanır; ölçüm plana "Sonuç" notu olarak yazılır.

| # | Kriter | Nasıl kanıtlanır |
|---|---|---|
| K1 | Postgres her gün kendiliğinden yedekleniyor, yedek **şifreli** ve **sunucunun dışında** | Ardışık 3 günün yedek nesneleri uzak depoda; dosyalar şifreli (açık metin `strings` ile okunmuyor) |
| K2 | Bir yedekten **boş bir cluster'a geri yükleme** yapılabiliyor ve uygulama o veriyi sunuyor | Tatbikat: yeni VM'de geri yükleme, geri yüklenen bir oyuncuyla login; **RPO** (en fazla ne kadar veri kaybı) ve **RTO** (geri gelme süresi) ölçülüp yazılıyor |
| K3 | Yedek alınamazsa ya da bayatlarsa biri **uyarılıyor** | Yedek job'u bilerek bozuluyor; uyarının geldiği süre ölçülüyor |
| K4 | k3s'in kendi durumu (datastore + `server/cred/`) ayrı ayrı yedekleniyor ve geri yüklenebiliyor (Bulgu #80) | Tatbikat VM'inde geri yükleme; Secret'lar okunabiliyor |
| K5 | `/health` dışarıdan düşerse ya da sertifikanın bitmesine **21 günden** az kalırsa uyarı geliyor | Kasıtlı bir kesinti (ör. gateway'i 0'a ölçeklemek) ve uyarının ulaşma süresi; hedef ≤ 5 dk |
| K6 | Prod'da metrikler toplanıyor ve **sadece tailnet'ten** görülebiliyor | Grafana tailnet'ten açılıyor; public IP'den ulaşılamıyor (port taraması) |
| K7 | Temel durumlar için uyarı var: pod hazır değil / restart döngüsü, disk > %80, bellek baskısı, sertifika süresi, yedek yaşı | Her kural için bir tetikleme denemesi (ya da `promtool test rules`) |
| K8 | Kapasite kararı ölçümle verildi (4 GB yeter mi, 8 GB mı) | Monitoring kurulduktan sonraki kullanım ve request toplamları; karar + gerekçe |
| K9 | Runbook güncel: geri yükleme, uyarıya müdahale, monitoring erişimi | `docs/deployment.md`'deki yeni bölümler bir tatbikatla izlenebiliyor |

## Ölçülen başlangıç durumu (2026-10-22)

- Node: 2 vCPU / 3,8 GiB; kullanım 244m CPU (%12), 2064 MiB (%52); request toplamı 1150m (%57) /
  1100 MiB (%28); disk %16 (62 GB).
- Postgres: 7,7 MB, 1 oyuncu (D3 testinin kullanıcısı). PVC'ler local-path, 1'er GB (Postgres,
  Redis, NATS).
- k3s datastore'u SQLite (kine): `/var/lib/rancher/k3s/server/db/`; şifreleme anahtarı
  `/var/lib/rancher/k3s/server/cred/` (aynı diskte, #80).
- `components/monitoring`: Prometheus v2.55 + Grafana 11.6, bir `ClusterRole` ile. CI'ın Role'ü
  RBAC yazamıyor; bu yüzden monitoring'in yetki kısmı **cluster seviyesinde** olmalı.
- E-posta: `ops@gamemesh.app` (Cloudflare Email Routing → kişisel kutu); domain'den **gönderim
  yok** (DMARC `p=reject`).

## Çalışma kuralları (Faz 1'den aynen)

- Her adım: önce açıklama ve kararlar, onay, sonra küçük Conventional Commit'ler; her iddia
  gerçek çıktıyla; kod okumasına dayanan iddia öyle etiketlenir.
- Sonuç notu ve kutu ayrı bir `docs(plan)` commit'inde.
- **Onay gerektirenler:** push, PR, merge; ücretli ya da dışa açık her işlem (yeni hesap/servis,
  depolama, DNS, sunucu büyütme); uzaktaki cluster'a her `kubectl apply/delete/exec`; araç
  kurulumu; GitHub ayarları.
- Secret değerleri asla ekrana basılmaz; repoya girmez.
- Prod'a sadece `kubectl apply -k deployments/k8s/overlays/prod`; cluster seviyesindeki
  manifest'ler k3s'in auto-deploy klasöründen.

---

## Bölüm E — Veri dayanıklılığı

### E1. Yedekleme stratejisi kararı

- [ ] **Ne:** Postgres için yöntem, uzak depo, şifreleme ve saklama süresi kararı.
- **Seçenekler:**
  | Yöntem | RPO | Karmaşıklık | Not |
  |---|---|---|---|
  | (a) Günlük `pg_dump` (CronJob) | ≤ 24 saat | Düşük | Mantıksal yedek; sürüm yükseltmelerinde taşınabilir; küçük veri için ideal |
  | (b) WAL arşivleme (WAL-G / pgBackRest) | dakikalar, PITR | Yüksek | Zamanda belirli bir ana dönme; sürekli arşiv akışı |
  | (c) Sadece netcup snapshot'ı | snapshot sıklığı | Düşük | Tutarlılık garantisi yok, aynı sağlayıcıda, Secret anahtarıyla birlikte (#80) |
- **Öneri:** Faz 2'de **(a)**; veri büyüyüp RPO önem kazanınca (b). (c) tek başına yedek
  sayılmaz.
- **Uzak depo seçenekleri** (S3 uyumlu): Cloudflare R2 (hesap zaten var; çıkış ücretsiz),
  Backblaze B2, Hetzner Storage Box. Fiyat ve ücretsiz kota **doğrulanacak**. Öneri: R2
  (tek sağlayıcı paneli, DNS ile aynı hesap; tek hesaba bağımlılık riski not edilir).
- **Şifreleme:** yedek sunucuda, yüklenmeden **önce** şifrelenir (ör. `age` public key ile).
  Özel anahtar sadece admin'de (parola yöneticisi + kağıt/çevrimdışı kopya); sunucuda **yok**.
  Böylece depo sızsa bile veri okunamaz; sunucu ele geçirilse bile eski yedekler çözülemez.
- **Erişim anahtarı:** depoya sadece bu bucket'a yazabilen bir token; tercihen silme yetkisi
  olmadan (object lock / retention destekleniyorsa) — fidye yazılımına karşı. Desteği
  **doğrulanacak**.
- **Saklama:** örn. 7 günlük + 4 haftalık + 3 aylık; depo yaşam döngüsü kuralıyla.
- **Öğrenilecek:** RPO/RTO, mantıksal ve fiziksel yedek farkı, 3-2-1 kuralı, istemci tarafı
  şifreleme, değiştirilemez yedek.
- **Bitti sayılır:** Kararlar plana yazıldı ve onaylandı.

### E2. Postgres yedek CronJob'u

- [ ] **Ne:** `gamemesh` namespace'inde günlük bir CronJob: `pg_dump --format=custom` →
  `age` ile şifrele → uzak depoya yükle → başarıda dead man's switch'e ping (F2).
- **Nasıl (taslak):**
  - Postgres'le **aynı sürümde** bir imaj (`postgres:16.x-alpine`, digest ile) ve yükleme
    için küçük bir araç (`rclone` ya da `aws-cli`), digest ile; ya da iki adımlı bir pod
    (initContainer dump alır, ikinci container yükler).
  - PSS `restricted` altında: non-root, read-only root filesystem, geçici veri `emptyDir`'de.
  - Depo erişim anahtarı ve `age` **public** key'i bir Secret'ta (`gamemesh-backup`), admin
    oluşturur; CI'ın Secret yetkisi yok.
  - `concurrencyPolicy: Forbid`, `startingDeadlineSeconds`, `successfulJobsHistoryLimit`,
    `activeDeadlineSeconds`; saat 03:30 UTC'deki otomatik reboot penceresiyle çakışmasın
    (ör. 02:15 UTC).
  - CronJob prod overlay'inde mi (CI yönetir) yoksa cluster seviyesinde mi? CI'ın Role'üne
    `batch/cronjobs` eklenmesi gerekir; karar bu adımda (en az yetki).
  - CI: imajlar üçüncü taraf taramasına girer; manifest kubeconform'dan geçer.
- **Öğrenilecek:** CronJob semantiği (kaçırılan çalışmalar, eşzamanlılık), pod güvenlik
  bağlamı, istemci tarafı şifreleme.
- **Bitti sayılır:** Elle tetiklenen bir job (`kubectl create job --from=cronjob/...`)
  uzak depoya şifreli bir dosya bıraktı; ertesi gün kendiliğinden bir tane daha geldi (K1).

### E3. k3s durumunun yedeği (#80)

- [ ] **Ne:** k3s datastore'u (SQLite) ve `server/cred/` + `server/token` için yedek; ikisi
  **ayrı** yerlerde.
- **Neden:** Datastore'da bütün Kubernetes nesneleri var (Secret'lar şifreli); anahtar
  `cred/`'de. İkisi birlikte sızarsa bütün Secret'lar açılır; ayrı tutulunca biri tek başına
  işe yaramaz. Postgres yedeği verinin kendisi; bu ise cluster'ı aynı Secret'larla geri
  getirmek için (aksi halde yeniden kurulumda Secret'lar yeniden üretilir ve Postgres
  parolası volume'daki ile uyuşmaz — Faz 1 runbook'una bakın).
- **Nasıl (taslak):** host üzerinde bir systemd timer: SQLite'ın tutarlı kopyası
  (`sqlite3 .backup` ya da k3s'in kendi aracı; **doğrulanacak**), şifrele, yükle. `cred/`
  için ayrı bir anahtar ve/veya ayrı depo; ya da `cred/` hiç otomatik yedeklenmez, admin
  çevrimdışı saklar (karar).
- **Bitti sayılır:** Yedek alındı; E5'te geri yükleme denendi (K4).

### E4. Yedek izleme

- [ ] **Ne:** Başarılı her yedek bir dead man's switch'e (F2) ping atar; ping gelmezse uyarı.
  Ayrıca Prometheus kuralı: son başarılı yedeğin yaşı (G4).
- **Bitti sayılır:** Job bilerek bozuldu (yanlış bucket adı); uyarı geldi, süresi ölçüldü
  (K3).

### E5. Geri yükleme tatbikatı

- [ ] **Ne:** Boş bir Ubuntu VM'de (Faz 1 D4'teki Multipass yöntemi) runbook'u izleyerek
  cluster kurmak, k3s durumunu ve Postgres yedeğini geri yüklemek, uygulamayı ayağa kaldırıp
  geri yüklenen bir hesapla giriş yapmak.
- **Ölçülecek:** RTO (kurulumdan servis verene kadar), RPO (son yedeğin yaşı), adım adım süre;
  runbook'taki eksikler (Faz 1 tatbikatında 5 tane çıkmıştı).
- **Dikkat:** Tatbikat cluster'ına `letsencrypt-prod` kurulmaz (Faz 1 D4 bulgusu).
- **Bitti sayılır:** K2 ve K4 kanıtlandı; runbook'a "Restoring from backup" bölümü eklendi.

## Bölüm F — Dış izleme ve uyarı

### F1. Dış izleme servisi ve uyarı kanalı

- [ ] **Ne:** Sunucunun ve ev ağının **dışında** çalışan bir izleme: `https://api.gamemesh.app/health`
  (200 ve `"status":"ok"`), bir proxy'lenen route (`/api/v1/leaderboard`), TLS sertifikasının
  kalan süresi.
- **Seçenekler:** UptimeRobot, Better Stack, Uptime Kuma (başka bir yerde barındırılırsa),
  zamanlanmış bir GitHub Actions workflow'u (kısa aralıklar için güvenilmez). Ücretsiz katman,
  kontrol aralığı, sertifika süresi kontrolü ve bildirim kanalları **doğrulanacak**.
- **Uyarı kanalı:** e-posta (`ops@gamemesh.app`), ve hızlı fark edilmesi için bir mobil
  bildirim (servisin uygulaması, Telegram ya da Discord webhook'u). Domain'den gönderim
  olmadığı için e-postayı servisin kendisi gönderir.
- **Bitti sayılır:** K5: gateway bilerek 0 replikaya indirildi (onaylı); uyarının ulaşma süresi
  ölçüldü; geri açılınca "düzeldi" bildirimi geldi.

### F2. Dead man's switch

- [ ] **Ne:** Yedek job'larının (E2, E3) "çalıştım" ping'i için bir servis (ör.
  healthchecks.io); beklenen aralıkta ping gelmezse F1'deki kanala uyarı.
- **Bitti sayılır:** E4.

### F3. Durum sayfası (isteğe bağlı)

- [ ] **Ne:** Dış izleme servisinin herkese açık durum sayfası; README'den link. Kök adres
  (`/`) için bir karşılama cevabı da burada değerlendirilir (Faz 1 sonrası not: CV'deki link
  şimdilik `/health`).

## Bölüm G — Prod'da monitoring

### G1. Kapasite ölçümü ve karar

- [ ] **Ne:** Monitoring yükünü ölçüp node'un yetip yetmediğine karar vermek.
- **Nasıl:** compose'da ölçülen tüketim (Prometheus ~95 MiB, Grafana ~217 MiB; Faz 1 M2) +
  kube-state-metrics, node-exporter, Alertmanager; prod'daki saklama süresiyle Prometheus
  belleği. Önce yerel k3d'de prod'a yakın ayarla ölçüm.
- **Seçenekler:** 4 GB'ta kalmak (saklama kısa, scrape aralığı 30 sn); netcup'ta 8 GB'lık
  plana geçmek (VPS 1000 G12.5; yerinde yükseltme mümkün mü, kesinti ve fiyat
  **doğrulanacak**); hafif bir alternatif (VictoriaMetrics single-node).
- **Bitti sayılır:** K8: karar ve gerekçe ölçümlerle yazıldı; gerekiyorsa büyütme (ücretli,
  onaylı) yapıldı ve reboot sonrası her şeyin geri geldiği ölçüldü.

### G2. Monitoring mimarisi

- [ ] **Ne:** Nerede ne çalışacak:
  - Uygulama metrikleri (`/metrics`) `gamemesh` namespace'inde.
  - Node ve cluster metrikleri (node-exporter hostPath/hostNetwork ister → PSS `restricted`'a
    uymaz) **ayrı bir `monitoring` namespace'inde**, uygun PSS seviyesiyle, admin tarafından.
  - Prometheus'un RBAC'ı: `gamemesh` için namespace'e sınırlı `Role` (Faz 1 sonrası notu),
    node/cluster için gerekli en dar `ClusterRole`; hepsi cluster seviyesinde (CI RBAC yazamaz).
  - Kurulum yolu: k3s `HelmChart` (kube-prometheus-stack gibi) ya da mevcut
    `components/monitoring`'in prod'a uyarlanması; sürümler sabit, imajlar digest ile.
  - Bulgu #68: Prometheus 3 / Grafana 12+ yükseltmesi bu adımda mı ayrı mı, karar.
- **Bitti sayılır:** Mimari kararlar plana yazıldı ve onaylandı.

### G3. Kurulum ve erişim

- [ ] **Ne:** Monitoring'i kurmak; Grafana'ya **sadece tailnet'ten** erişim (port-forward
  ya da tailnet'e bağlı bir erişim yolu); public Ingress yok (admission policy zaten yalnızca
  `api.gamemesh.app`'e izin veriyor). Grafana admin parolası Secret'tan.
- **Bitti sayılır:** K6: Grafana tailnet'ten açılıyor; public IP'de ilgili portlar TIMEOUT;
  dashboard'larda uygulama, node ve cert-manager metrikleri.

### G4. Uyarı kuralları ve yönlendirme

- [ ] **Ne:** Alertmanager kuralları: `KubePodNotReady`/crashloop, disk > %80, bellek baskısı
  (OOMKilled dahil), sertifika süresi (cert-manager metriği) < 21 gün, son başarılı yedeğin
  yaşı > 26 saat, hedef düşmesi (scrape başarısız). Yönlendirme F1'deki kanala (webhook ya da
  e-posta servisi).
- **Bitti sayılır:** K7: kurallar `promtool test rules` ile test edildi ve en az bir kural
  gerçekten tetiklenip bildirim ulaştı.

### G5. Runbook

- [ ] **Ne:** `docs/deployment.md`: "Restoring from backup", "Monitoring access", "When an alert
  fires" bölümleri; her uyarı için ilk bakılacak yer.
- **Bitti sayılır:** K9; E5 tatbikatı bu bölümlerle yapıldı.

## Bölüm H — Tarihli bakım (Faz 1'den)

| Tarih | İş | Bulgu |
|---|---|---|
| 2026-11-04 | `nats-server` CVE-2026-111943 kaydı bitiyor: yamalı NATS (Dependabot) gelirse güncelle ve kaydı sil; gelmezse yeniden değerlendir | #88 |
| 2026-11-18 | Redis imajındaki OpenSSL CVE-2026-84782 kaydı bitiyor | — |
| 2026-12-20 | `api-tls`'in ilk otomatik yenilemesi (admission policy altında): `kubectl -n gamemesh get certificate` ile doğrula; F1'deki sertifika kontrolü de görmeli | #85, #87 |
| 2027-01-18 | gosu Go CVE kayıtları bitiyor | — |
| 2027-04-21 | KSV-0056 kaydı bitiyor | #87 |

## Kapsam dışı (Faz 3 ya da sonrası)

- Yüksek erişilebilirlik (birden fazla node, yönetilen veritabanı).
- Merkezi log toplama (Loki vb.); bugün `kubectl logs` ve kısa saklama.
- arm64 imaj taraması (#78), cert-manager imajlarının taranması (#84), `secretbox` (#80), JWT
  `kid` rotasyonu, k3s 1.36, dual-stack (#81), matchmaking için leader lock: Faz 1 planının
  "Faz 1 sonrası" listesinde; Faz 2'nin kapsamı onlara dokunuyorsa birlikte ele alınır.

## Bulgu dizini

Faz 1'in dizini 89'da bitti; Faz 2'nin bulguları **#90**'dan başlar ve Faz 1 planındaki
tabloyla aynı biçimde buraya eklenir.

| # | Bulgu | Kaynak | Kanıt | Adım |
|---|---|---|---|---|
