# Deployment Faz 2 — Gözlemlenebilirlik ve gerçek kullanıcıya hazırlık

**Hedef:** Prod'da ne olduğu **görülebiliyor** (metrikler, dashboard, herkese açık bir durum
sayfası), sistem verisini kaybetmeden ve bozulduğunda haber vererek gerçek kullanıcı
taşıyabiliyor. Faz 1 sistemi canlıya aldı (`docs/deployment-phase1-plan.md`); Faz 2:

1. **Prod'da monitoring (Bölüm G, danışman kapsamı):** Prometheus, uygulama metrikleri,
   Grafana'da 4 panel, `status.gamemesh.app`'te herkese açık salt okunur dashboard, k6 ile
   doğrulama. Bugün Prometheus/Grafana sadece compose'da ve yerel k3d'de (Bulgu #68).
2. **Veri dayanıklılığı (Bölüm E):** Bugün verinin tek kopyası sunucunun diskinde.
3. **Dış izleme (Bölüm F):** Sistem çökerse ya da sertifika yenilenmezse bunu ilk öğrenen
   kullanıcı olur (Bulgu #85); ev ağından yapılan ölçümler bazen yanıltıcı (Bulgu #83).

Faz 1'den kalan **tarihli işler** tarihlerinde yapılıyor (Bölüm H).

**Kaynaklar:** Bölüm G'nin kapsamı ve "dikkat edilecek üç nokta" danışmanın kriterlerinden
(2026-10-22). E, F, H ve G'deki kapasite/runbook adımları Faz 1 kapanışında eklenen "gerçek
kullanıcılardan önce şart olanlar"dan; kullanıcı bunların korunmasını istedi.

> **Karar (2026-10-22): Alertmanager ve uyarı kuralları Faz 3'e.** Danışman bunları kapsam dışı
> sayıyordu, önceki plan içeriyordu (K7, G8). Kullanıcı danışmana uymayı seçti: K7 ve G8 Faz
> 3'e taşındı (numaralar boş kalıyor), README'de roadmap maddesi. Gerekçe: en kritik iki uyarı
> Alertmanager'dan bağımsız olarak Faz 2'de zaten var: sitenin ve sertifikanın dış izlemesi (F1,
> K5) ve yedeklerin dead man's switch'i (F2, K3).

## Tamamlanma kriterleri

Her kriter bir ölçümle kanıtlanır; ölçüm plana "Sonuç" notu olarak yazılır. Numaralar sabit
(önceki planla aynı K1–K9, danışman kapsamı K10–K15).

### Danışman kapsamı (Bölüm G)

| # | Kriter | Nasıl kanıtlanır |
|---|---|---|
| K10 | Prometheus cluster içinde; 7 servisin `/metrics`'i scrape ediliyor, **bütün target'lar UP** | Prometheus'un target sayfası / `up` sorgusu: 7/7 = 1 (ekran görüntüsü + sorgu çıktısı) |
| K11 | Uygulama metrikleri: istek sayacı, gecikme histogramı, hata sayacı, aktif WebSocket gauge'ı; **label'lar sınırlı** ve **bucket'lar gecikme aralığına göre** | Metrik listesi; bir test kardinalitenin sınırlı kaldığını gösteriyor (uydurma method/yol yeni seri açmıyor, Bulgu #90); bucket'lar ölçülen p95/p99 etrafında sık |
| K12 | Grafana'da 4 panel: istek hızı (req/s), p95/p99 gecikme, hata oranı (%), aktif bağlantı | Dashboard repoda kod olarak (JSON, provisioning); panellerin PromQL'i plana yazılı |
| K13 | `status.gamemesh.app` dışarıdan açılıyor, **giriş istemiyor, düzenleme kapalı** | Anonim tarayıcıdan dashboard görünüyor; düzenleme/kaydetme ve admin API'leri reddediliyor (HTTP kodları); Prometheus dışarıdan erişilemiyor (port taraması, `/api/v1/query` 404/timeout) |
| K14 | k6 ile yük basıldığında paneller gerçek zamanlı hareket ediyor | Yük sırasında alınmış ekran görüntüsü; panellerdeki değerler k6'nın raporuyla tutarlı |
| K15 | Kapsam dışı bırakılanlar README'de "Roadmap" olarak | README bölümü (kodda değil) |

### Gerçek kullanıcıya hazırlık (E, F ve G'nin kapasite/runbook adımları)

| # | Kriter | Nasıl kanıtlanır |
|---|---|---|
| K1 | Postgres her gün kendiliğinden yedekleniyor, yedek **şifreli** ve **sunucunun dışında** | Ardışık 3 günün yedek nesneleri uzak depoda; dosyalar şifreli (açık metin `strings` ile okunmuyor) |
| K2 | Bir yedekten **boş bir cluster'a geri yükleme** yapılabiliyor ve uygulama o veriyi sunuyor | Tatbikat: yeni VM'de geri yükleme, geri yüklenen bir oyuncuyla login; **RPO** ve **RTO** ölçülüp yazılıyor |
| K3 | Yedek alınamazsa ya da bayatlarsa biri **uyarılıyor** | Yedek job'u bilerek bozuluyor; uyarının geldiği süre ölçülüyor |
| K4 | k3s'in kendi durumu (datastore + `server/cred/`) ayrı ayrı yedekleniyor ve geri yüklenebiliyor (Bulgu #80) | Tatbikat VM'inde geri yükleme; Secret'lar okunabiliyor |
| K5 | `/health` dışarıdan düşerse ya da sertifikanın bitmesine **21 günden** az kalırsa uyarı geliyor | Kasıtlı bir kesinti ve uyarının ulaşma süresi; hedef ≤ 5 dk |
| K6 | Prometheus ve Grafana'nın **yönetimi** sadece tailnet'ten; dışarı açık tek şey K13'teki salt okunur dashboard | Grafana admin girişi sadece tailnet'ten; public IP'de Prometheus ve Grafana admin yolları kapalı |
| ~~K7~~ | *Faz 3'e taşındı (karar 2026-10-22).* Alertmanager ile uyarı kuralları: pod hazır değil / restart döngüsü, disk > %80, bellek baskısı | — |
| K8 | Kapasite kararı ölçümle verildi (4 GB yeter mi, 8 GB mı) | Monitoring kurulduktan sonraki kullanım ve request toplamları; karar + gerekçe |
| K9 | Runbook güncel: geri yükleme, monitoring erişimi, durum sayfası, dış izleme ve yedek uyarısına (F1, F2) müdahale | `docs/deployment.md`'deki yeni bölümler bir tatbikatla izlenebiliyor |

## Tasarım ilkeleri (danışmanın üç noktası; aynı zamanda mülakat cevapları)

**"Neden bu metrikleri seçtin, kardinaliteyi nasıl sınırladın, p95'i nasıl hesaplıyorsun?"**

1. **Güvenlik.** Grafana'ya anonim erişim açılırsa rolü **Viewer** olmalı; admin paneli ve
   düzenleme dışarı açılmaz. **Prometheus dışarı hiç açılmaz:** ham metrik ve sorgu
   endpoint'leri iç yapıyı (route'lar, hata oranları, kuyruk boyutları) anlatır ve pahalı
   sorgularla kaynak tüketmeye açıktır. Herkese açık dashboard'un her izleyicisi Prometheus'a
   sorgu demektir; yenileme aralığının alt sınırı ve bir rate limit gerekir.
2. **Kardinalite.** Label'lar sadece **sınırlı** kümelerden değer alır: route şablonu (ham yol
   değil; eşleşmeyenler `unmatched`), durum kodu, servis. `user_id`, `match_id`, `room_id`,
   `jti` gibi sınırsız değerler asla label olmaz (gerekirse log ve trace'te durur). Bugün
   **`method` label'ı sınırsız** (Bulgu #90): uydurma bir HTTP method her seferinde yeni bir seri
   açıyor ve Traefik bunu internetten gateway'e iletiyor. Düzeltme Prometheus kurulmadan önce.
3. **Histogram.** p95/p99 `histogram_quantile` ile, bucket'ların `rate`'inden ve servis/route
   üzerinden toplanarak (`sum by (le)`) hesaplanır. Bugünkü bucket'lar Prometheus'un
   varsayılanı (`…, .05, .1, .25, …`): 112 ms civarında tek bir aralık (100–250 ms) var ve
   p95 bu aralıkta doğrusal interpolasyonla tahmin ediliyor, yani kaba. Bucket'lar ölçülen
   gecikme dağılımına göre bu bölgede sıklaştırılır (ör. 50, 75, 100, 125, 150, 200, 300 ms);
   bucket sayısı da her seriyi çoğalttığı için kardinalite bütçesine dahildir.

## Ölçülen başlangıç durumu (2026-10-22)

- Node: 2 vCPU / 3,8 GiB; kullanım 244m CPU (%12), 2064 MiB (%52); request toplamı 1150m (%57) /
  1100 MiB (%28); disk %16 (62 GB).
- Postgres: 7,7 MB, 1 oyuncu (D3 testinin kullanıcısı). PVC'ler local-path, 1'er GB.
- k3s datastore'u SQLite (kine): `/var/lib/rancher/k3s/server/db/`; şifreleme anahtarı
  `/var/lib/rancher/k3s/server/cred/` (aynı diskte, #80).
- **Metrikler (kod okuması + yerel ölçüm):** `gamemesh_http_requests_total` ve
  `gamemesh_http_errors_total` (`method`, `path`, `status`), `gamemesh_http_request_duration_seconds`
  (`method`, `path`; varsayılan bucket'lar), `gamemesh_websocket_active_connections`; ayrıca
  matchmaking, leaderboard, event, presence ve outbox metrikleri. `path` route şablonu (sınırlı);
  **`method` sınırsız**: yerelde 25 uydurma method 25 yeni seri açtı; prod'da Traefik
  `ZZCHECK1` method'unu gateway'e iletti (gateway log'u: `"method":"ZZCHECK1"`, 404).
- `components/monitoring`: Prometheus v2.55 + Grafana 11.6, bir `ClusterRole` ile. CI'ın Role'ü
  RBAC yazamıyor; monitoring'in yetki kısmı **cluster seviyesinde** olmalı.
- Admission policy (`gamemesh-network-surface`) `gamemesh` namespace'inde Ingress host'unu
  sadece `api.gamemesh.app`'e izin veriyor; `status.gamemesh.app` için bilinçli bir genişletme
  gerekir (G6).
- Rate limit: gateway istemci IP'si başına 50 rps (burst 100); k6'nın tek IP'den basacağı yük
  bunu aşarsa limiter'ı ölçer (Faz 1 README notu).
- E-posta: `ops@gamemesh.app` (Cloudflare Email Routing → kişisel kutu); domain'den gönderim yok
  (DMARC `p=reject`).

## Çalışma kuralları (Faz 1'den aynen)

- Her adım: önce açıklama ve kararlar, onay, sonra küçük Conventional Commit'ler; her iddia
  gerçek çıktıyla; kod okumasına dayanan iddia öyle etiketlenir.
- Sonuç notu ve kutu ayrı bir `docs(plan)` commit'inde.
- **Onay gerektirenler:** push, PR, merge; ücretli ya da dışa açık her işlem (yeni hesap/servis,
  depolama, DNS kaydı, sunucu büyütme); uzaktaki cluster'a her `kubectl apply/delete/exec`;
  araç kurulumu; GitHub ayarları.
- Secret değerleri asla ekrana basılmaz; repoya girmez.
- Prod'a sadece `kubectl apply -k deployments/k8s/overlays/prod`; cluster seviyesindeki
  manifest'ler k3s'in auto-deploy klasöründen.

**Önerilen sıra:** G3 (Bulgu #90 ve bucket'lar; kod) → G1 → G2 → G4 → G5 → G6 → G7 → G9;
paralelde E ve F; H tarihlerinde. (G8 Faz 3'te.)

---

## Bölüm G — Prod'da monitoring (danışman kapsamı + kapasite)

### G1. Kapasite ölçümü ve karar

- [x] **Ne:** Monitoring yükünü ölçüp node'un yetip yetmediğine karar vermek.
- **Nasıl:** compose'da ölçülen tüketim (Prometheus ~95 MiB, Grafana ~217 MiB; Faz 1 M2) +
  seçilen yığının diğer bileşenleri (kube-state-metrics, node-exporter; Alertmanager yok, Faz 3);
  saklama süresiyle Prometheus belleği. Önce yerel k3d'de prod'a yakın ayarla ölçüm.
- **Seçenekler:** 4 GB'ta kalmak (kısa saklama, 30 sn scrape); netcup'ta 8 GB'lık plana geçmek
  (VPS 1000 G12.5; yerinde yükseltme, kesinti ve fiyat **doğrulanacak**).
- **Bitti sayılır:** K8.

> **Ön sonuç (2026-10-23, yerel k3d'de ölçüldü): 4 GB yeter, sade yığın şartıyla.** İki yığın aynı
> uygulama trafiği altında, ikisi de 7–10 dakikalıkken (uzun süreli ölçüm yok; head ~2 saatte
> dolar):
>
> | | Sade (Prometheus 2.55 + Grafana 11.6) | kube-prometheus-stack 92.11 (Prometheus 3.15, Grafana 13.2) |
> |---|---|---|
> | Seri | 1616–1837 (hepsi uygulama) | 79.758–114.096 (uygulama %1,6–2) |
> | Bellek (working set) | Prometheus 53–99, Grafana 110–111 → **164–209 Mi** | Prometheus 444–454, Grafana 491–606, operator 41–44, KSM 38–41, node-exporter 14–20 → **1038–1155 Mi** |
> | Prod'da kalacak `available` (bugün 2313 Mi) | ~2,1 GiB | ~1,2 GiB ve head büyüdükçe azalır |
>
> kube-prometheus-stack'te serilerin çoğu kubelet (54.262 `/metrics`, 5458 cAdvisor) ve apiserver
> (26–39 bin); k3s'te ikisi aynı kontrol düzlemi metrikleri (Bulgu #93). k3d'de 34 pod var, prod'da
> 18: kubelet serileri prod'da daha az. **Ölçütler (onaylandı):** 15 sn scrape, 7 gün + 2 GB saklama;
> monitoring sonrası prod'da `available` < 1 GiB, bir OOMKilled ya da bellek request'leri
> allocatable'ın %75'ini geçerse VPS 1000 G12.5'e (€14,50/ay, şu an €8,26; G12.5 içinde yerinde
> yükseltme; kesinti ve disk büyümesi **doğrulanacak**). K8'in kanıtı G4'ten sonra prod'da.
>
> **Sonuç (2026-10-25, K8): 4 GB yeter, büyütme yok.** Prod'da Prometheus ve Grafana çalışırken,
> G7'nin yükünün zirvesinde (30 istek/sn, 20 WebSocket): `available` **2121 Mi** (eşik 1 GiB),
> bellek request'leri **%37** (1468 Mi; eşik %75), limitler %80, OOMKilled yok, load 1,04. Prometheus
> 1,5 gündür çalışıyor ve 48 Mi, Grafana 100 Mi. Önceki okumalar: 2313 (öncesi), 2273 (G4), 2127 Mi
> (G5).

### G2. Monitoring mimarisi

- [x] **Ne:** Nerede ne çalışacak; kararlar:
  - **Yığın (danışman madde 1):** kube-prometheus-stack (Prometheus Operator, Grafana,
    kube-state-metrics, node-exporter; büyük, CRD'li) mı, sade Prometheus + Grafana mı
    (mevcut `components/monitoring`'in prod'a uyarlanması)? Önerinin gerekçesi: bellek (G1),
    Faz 2'nin hedefi (uygulama paneli; node metrikleri ikincil), ve sade olanın okunabilirliği.
  - **Namespace:** monitoring ayrı bir `monitoring` namespace'inde. node-exporter hostPath/
    hostNetwork ister → PSS `restricted`'a uymaz; kullanılacaksa namespace'in PSS seviyesi buna
    göre ve bilinçli.
  - **RBAC:** Prometheus'un `gamemesh`'i scrape etmesi için namespace'e sınırlı `Role` (Faz 1
    sonrası notu), cluster metrikleri için gereken en dar `ClusterRole`; hepsi cluster
    seviyesinde (CI RBAC yazamaz).
  - **Kurulum yolu:** k3s `HelmChart` ya da düz manifest; sürümler sabit, imajlar digest ile,
    üçüncü taraf taramasına dahil.
  - **Bulgu #68:** Prometheus 3 / Grafana 12+ yükseltmesi bu adımda mı, ayrı mı.
  - **Saklama:** kısa (ör. 7 gün; uzun süreli saklama kapsam dışı).
- **Bitti sayılır:** Kararlar plana yazıldı ve onaylandı.

> **Kararlar (2026-10-23, onaylandı):**
>
> | # | Konu | Karar | Gerekçe |
> |---|---|---|---|
> | 1 | Yığın | **Sade** Prometheus + Grafana | G1: 5–7 kat daha az bellek; kube-prometheus-stack'in serilerinin %98'i uygulama dışı (#93); operator ve CRD'ler 4 uygulama paneline bir şey katmıyor. Node/cluster panelleri kapsam dışı ("ek paneller") |
> | 2 | Sürümler (#68) | **Prometheus 3.13.x (LTS, destek 2027-07-31)**, **Grafana 13.2.x** (destek ~2027-05-18); compose ve Kubernetes aynı PR'da | 2.55 hiç LTS olmadı (2.53'ün desteği 2025-07-31'de bitti); 11.6'nın desteği 2026-06-25'te bitti. Risk: Prometheus 3'te `le` değerleri normalize (`"1"` → `"1.0"`); Grafana 13'ün belleği k3d'de ölçülecek |
> | 3 | Namespace | `monitoring`, PSS `restricted` | node-exporter yok; G6'nın ayrı admission kuralına yer |
> | 4 | RBAC | `gamemesh`'te `Role` (pods get/list/watch) + `monitoring/prometheus` SA'sına `RoleBinding`; ClusterRole yok | En az yetki (Faz 1 sonrası maddesi) |
> | 5 | Kim kurar | Namespace, RBAC, admission: cluster seviyesi (admin). Workload'lar (Deployment, ConfigMap, Service, PVC): CI; CI'a `monitoring`'de RBAC ve Secret yetkisi olmayan yeni bir Role | Dashboard'lar kod olarak sık değişir; her değişiklik için `sudo` pratik değil |
> | 6 | Depolama | PVC (local-path): Prometheus 3 Gi, Grafana 1 Gi | Public dashboard'un ayarı ve token'ı Grafana'nın veritabanında; PVC'siz her yeniden başlatmada adres değişir (dosyayla provision edilip edilemediği G6'da doğrulanacak) |
> | 7 | Grafana admin | `kprod port-forward` (tailnet); admin için Ingress yok; parola `grafana-admin` Secret'ında (admin oluşturur) | Admin yüzeyi dışarıda hiç yok |
> | 8 | Scrape | Uygulama pod'ları + Prometheus'un kendisi | cAdvisor (~5,4 bin seri) roadmap |
> | 9 | Alarm kuralları | Prod'da yok (Faz 3); yerel overlay'de kalabilir | Danışmanın kapsamı |
>
> **2. kararın revizyonu (2026-10-23, onaylandı; Bulgu #94):** CI'ın engelleyici kurallarıyla
> (HIGH/CRITICAL, düzeltmesi olan, `.trivyignore.yaml`) taranınca Prometheus `v3.13.5-distroless`'ta
> CVE-2026-84445 (grpc) çıktı, `v3.15.1-distroless` temiz: **Prometheus 3.15.1** (LTS değil;
> Dependabot minor'ları haftalık öneriyor). Temiz bir Grafana yok (13.2.4: 75, 13.1.8: 18, 12.4.13: 8
> bulgu; hepsinde Go'nun CVE-2026-111943'ü): **Grafana kararı G5'e**; o zamana kadar 11.6.16 sadece
> yerelde ve rapor modunda. G5'te Go ≥ 1.26.10 ile derlenmiş bir sürüm yoksa 12.4.x + dar, süreli
> istisnalar (x509: Grafana dışarıya TLS açmaz; tempo: Tempo datasource'u yok; kin-openapi:
> erişilebilirlik doğrulanacak, G6'nın yol filtresi yüzeyi daraltır) ve 12.4'te public dashboard
> doğrulaması.
>
> **G6 (c) doğrulandı (yerel, Grafana 11.6.16 ve 13.2.4 OSS):** public dashboard varsayılan olarak
> açık; `/public-dashboards/<token>`, `/api/public/dashboards/<token>` ve panel sorgusu anonim 200,
> `/api/dashboards/uid/...`, `/api/search`, `/api/admin/settings` ve `POST /api/dashboards/db` 401.
> Tek Grafana yeter ve anonim giriş hiç açılmaz. G6'nın önerisi buna göre (c).

### G3. Uygulama metrikleri (danışman madde 2) ve Bulgu #90

- [x] **Ne:**
  - Mevcut metriklerin danışmanın listesini karşıladığını doğrulamak: istek sayacı, gecikme
    histogramı, hata sayacı, aktif WebSocket gauge'ı (var; isimler ve label'lar plana).
  - **Bulgu #90:** `method` label'ını sınırlı bir kümeye indirmek (`GET`, `POST`, `PUT`,
    `PATCH`, `DELETE`, `HEAD`, `OPTIONS`, geri kalanı `other`); `status` için de beklenmeyen
    değerleri sınırlamak gerekip gerekmediğine bakmak. **Bir test:** uydurma method'lar ve
    rastgele yollar seri sayısını artırmıyor.
  - **Bucket'lar:** yerelde ve prod'da ölçülen gecikme dağılımına (p50/p95/p99) göre yeni
    sınırlar; değişikliğin seri sayısına etkisi (bucket × route × method × servis) hesaplanıp
    yazılıyor. Hata oranı için tanım: 5xx mi, 4xx dahil mi (karar; 429 ve 404 ayrı düşünülmeli).
- **Öğrenilecek:** kardinalite bütçesi, histogram ve summary farkı, `histogram_quantile`'ın
  doğrusal interpolasyonu ve hata payı, native histogramlar (alternatif olarak, kullanılmasa da).
- **Bitti sayılır:** K11; test CI'da geçiyor; prod'a deploy edildi ve uydurma bir method yeni
  seri açmıyor (ölçüm).

> **Sonuç (2026-10-23): bitti (PR #91, prod'da `94a1358`).**
>
> **Ölçülen gecikme (sunucu tarafı, gateway):** prod boşta p50 1,4–1,6 ms, p95 2,2–3,3 ms;
> yerelde README'nin matchmaking yükünde (500 VU) auth dışı route'lar p50 3,2, p95 16,2, p99
> 30,2 ms; `auth/login` ve `register` 200–240 ms (bcrypt). README'deki "112 ms p95" istemci
> tarafı ve bir kenar etkisi (Bulgu #91).
>
> **Kararlar:**
>
> | Konu | Karar | Gerekçe |
> |---|---|---|
> | `method` | `GET/POST/PUT/PATCH/DELETE/HEAD/OPTIONS`, gerisi `other` | İstemci girdisi; gin bilinmeyen method'u `unmatched` 404'e düşürüyor, sınır orada da lazım |
> | `status` | Tam kod | Değeri bizim kodumuz üretiyor (sonlu); 404/429/401 ayrımı gerekli |
> | WebSocket | Hijack edilen istek `101` sayılır, histograma girmez | Gateway'de süre = oturumun ömrü (Bulgu #92) |
> | Hata | `gamemesh_http_errors_total` sadece 5xx; hata oranı = 5xx / toplam, **gateway'de** | 404 normal akış (`DELETE /queue`), 401/404'ü herkes üretir, 429 politika; 502/503/504 dahil |
> | Bucket'lar | 1, 2,5, 5, 10, 15, 20, 30, 50, 75, 100, 150, 200, 250, 300, 500 ms, 1, 2,5, 5 s | Ölçülen p95/p99 bölgesinde sık; aynı 19.000 istekte p95 hatası %29 → %3, p99 %18 → %1 |
> | Panel katmanı | Kullanıcının gördüğü paneller `service="gateway"` | Her istek gateway'de ve upstream'de iki kez sayılıyor (20.000 k6 isteği = 40.000 gözlem) |
>
> **Seri bütçesi (hesap):** method/route çifti başına 21 histogram serisi (önce 14); bütün
> servislerde üst sınır 91 çift = 1911 seri. Önce sınırsızdı: her yeni method gateway'de 16 seri.
>
> **Kanıt:** Testler eski kodda kırmızı, yenide yeşil: 100 uydurma method 102/101/102 seri →
> 3/0/3 (istek/hata/histogram); WS testi; 5xx eşiği; 19 bucket. Gerçek compose'da 11 k6
> WS oturumu iki serviste `101`, `/ws` histogram serisi yok; token'sız el sıkışma 401 ve süresi
> kayıtlı. **Prod** (port-forward ile gateway `/metrics`): seri sayısı 22 → `ZZCHECK2…6` ile 44
> (`other` için bir kerelik 21 histogram + 1 sayaç) → `QQPROBE1…5` ve rastgele yollarla **44**;
> `requests_total{method="other",path="unmatched",status="404"} 10`, label'larda ham method ya
> da yol yok; route'larda 19 bucket sınırı (18 + `+Inf`); 4xx'ler `errors_total`'a girmedi.
>
> **Kapsam dışı kalan:** event ve outbox histogramlarının bucket'ları değişmedi; native
> histogram (Prometheus 2.55'te feature flag) sadece alternatif olarak not edildi. İki tepeli
> dağılımda p95, auth istekleri toplamın ~%5'i olduğunda uçurumun kenarına düşer; hiçbir bucket
> seti bunu çözmez, panel okunurken bilinmeli.

### G4. Prometheus kurulumu (danışman madde 1)

- [x] **Ne:** Prometheus'u G2'deki mimariyle kurmak; 7 servisin `/metrics`'ini scrape etmek
  (ServiceMonitor ya da `kubernetes_sd_configs`); Prometheus Service'i `ClusterIP`, Ingress yok.
- **Bitti sayılır:** K10: `up{namespace="gamemesh"}` 7/7 = 1; Prometheus'a dışarıdan
  ulaşılamıyor (port taraması, `api.gamemesh.app/api/v1/query` → 404).

> **Sonuç (2026-10-23): bitti (PR #94, #95; prod'da `a407dba`).** Prometheus 3.15.1 (#94 tarama
> sonucu) `monitoring` namespace'inde (PSS restricted), 15 sn scrape, 7 gün / 2 GiB, 3 Gi PVC; CI
> uygulamadan sonra kuruyor (merge → prod 7,5 dk; Prometheus rollout 12 sn). Cluster seviyesindeki
> dosyalar (namespace, Prometheus'un SA + `gamemesh`'te Role, admission policy, CI'ın `monitoring`
> Role'ü) admin tarafından kuruldu; k3s'in checksum'ları repodakilerle aynı.
>
> **Kanıt:**
> - **K10:** port-forward ile `up{namespace="gamemesh"}` 7 servisin 7'si = 1, `up == 0` boş,
>   Prometheus kendini scrape ediyor; head 1328 seri, RSS 93 MiB.
> - **Dışarıdan erişilemiyor:** public adreste sadece 80 ve 443 açık (22, 6443, 9090, 3000,
>   10250, 30000–32767 kapalı); `api.gamemesh.app` üzerinden `/api/v1/query`, `/api/v1/targets`,
>   `/metrics`, `/-/ready`, `/graph` → 404; başka Host başlıklarıyla (`prometheus.gamemesh.app`,
>   `status.gamemesh.app`) → 404.
> - **Politika (K6'nın yarısı, `--dry-run=server`, prod):** `monitoring`'de Ingress cluster-admin'e
>   bile reddediliyor ("Ingresses are not allowed"), CI'a RBAC'la; NodePort ve LoadBalancer
>   reddediliyor; ClusterIP'ye izin var; hiçbir şey kaydedilmedi.
> - **Yetkiler:** CI'ın beklenen 16 hakkı (`auth can-i --as`) tutuyor; Prometheus sadece
>   `gamemesh`'te pod listeleyebiliyor, Secret okuyamıyor.
> - **Yerel prova (k3d):** monitoring CI'ın kimliğiyle uygulandı; `deploy_monitoring` üç durumda
>   (monitoring'li commit, monitoring'siz commit, NodePort eklenmiş ağaç → rc 1, hiçbir şey
>   uygulanmadı) doğru.
>
> **K8 için ilk ölçüm (Prometheus ~4 dk'lık):** `available` 2273 Mi (önce 2313), bellek
> request'leri 1292 Mi (%33), OOMKilled yok, disk %21. Kesin ölçüm G5'ten (Grafana) sonra ve
> birkaç saatlik çalışmanın ardından.

### G5. Grafana ve 4 panel (danışman madde 3)

- [x] **Ne:** Grafana (admin parolası Secret'tan, admin girişi sadece tailnet), datasource ve
  dashboard **kod olarak** (provisioning, JSON repoda). Panellerin PromQL'i (taslak):
  Hepsi `service="gateway"` ile (G3: her istek upstream'de bir kez daha sayılıyor):
  - **İstek hızı:** `sum(rate(gamemesh_http_requests_total{service="gateway"}[1m]))`
    (servise göre kırılım isteğe bağlı).
  - **p95/p99:** `histogram_quantile(0.95, sum by (le) (rate(gamemesh_http_request_duration_seconds_bucket{service="gateway"}[5m])))`
    ve aynısı `0.99` ile. WS oturumları histogramda yok (G3, #92).
  - **Hata oranı (%):** `100 * sum(rate(gamemesh_http_errors_total{service="gateway"}[5m])) / sum(rate(gamemesh_http_requests_total{service="gateway"}[5m]))`
    (G3: `errors_total` sadece 5xx).
  - **Aktif bağlantı:** `sum(gamemesh_websocket_active_connections)`.
- **Bitti sayılır:** K12.

> **Sonuç (2026-10-24): bitti (PR #97, prod'da `e32bcc8`).** Grafana 12.4.13 `monitoring`'de
> (PSS restricted, 1 Gi PVC, ClusterIP), CI uygulamadan sonra kuruyor (rollout ~30 sn). Datasource ve
> dashboard repodan provision ediliyor (`deployments/k8s/monitoring/grafana/`); compose aynı
> dashboard dosyasını ve `grafana.ini`'yi kullanıyor. Admin parolası `grafana-admin` Secret'ında
> (admin oluşturdu; 64 hex).
>
> **Paneller (K12), hepsi `[1m]`:**
>
> | Panel | PromQL |
> |---|---|
> | İstek hızı | `sum(rate(gamemesh_http_requests_total{service="gateway"}[1m]))` |
> | p95 / p99 | `histogram_quantile(0.95 \| 0.99, sum by (le) (rate(gamemesh_http_request_duration_seconds_bucket{service="gateway"}[1m])))` |
> | Hata oranı (%) | `100 * (sum(rate(gamemesh_http_errors_total{service="gateway"}[1m])) or vector(0)) / sum(rate(gamemesh_http_requests_total{service="gateway"}[1m]))` |
> | Aktif bağlantı | `sum(gamemesh_websocket_active_connections{service="websocket"})` |
>
> **Kararlar (onaylandı):** 12.4.13 + 9 dar, 2026-11-10'da biten istisna (#94; temiz Grafana sürümü
> yok); dışarıya hiç bağlantı yok (güncelleme/eklenti/haber kontrolleri, kullanım raporu, eklenti
> kataloğu ve preinstall, imza anahtarı indirme, harici snapshot, Gravatar kapalı); anonim kapalı;
> `min_refresh_interval` 10 sn; panellerde sadece gateway (G3: her istek upstream'de bir daha sayılıyor).
>
> **Kanıt:**
> - **Tarama:** CI kurallarıyla istisnalarla 0 bulgu (istisnasız 9); CI'da `scan-third-party
>   (grafana/grafana:12.4.13)` engelleyici modda geçti.
> - **Dışarıya bağlantı:** ağsız bir container'da varsayılan ayarlar grafana.com'dan imza
>   anahtarı indirmeye ve **4 eklenti kurmaya** çalıştı; `grafana.ini` ile hiç deneme yok. Prod log'unda
>   da 0.
> - **Prod (port-forward):** admin 200, yanlış parola 401, anonim (search, dashboard, datasource,
>   admin) 401; dashboard ve salt okunur datasource provision edilmiş; admin bile provision edilmiş
>   dashboard'un üzerine kaydedemiyor ("Cannot save provisioned dashboard"); 8 ayar devrede.
>   Hafif trafik altında paneller: istek hızı en çok 1,47/s (gönderilen ~1,3–2/s), p95 en çok 7,8 ms,
>   p99 13,6 ms, hata 0.
> - **Dışarıdan erişilemiyor:** 3000 kapalı; `api.gamemesh.app` üzerinden Grafana yolları
>   (`/login`, `/api/health`, `/public-dashboards/…`, `/api/public/…`, `/d/…`) gateway'in 404'ü;
>   `grafana.`/`status.` Host'ları 404.
> - **Yerel (k3d, k6 40 matchmaking + 20 WS VU):** istek hızı 11,6–15,1/s, p95 7,9–9,6 ms, hata 0,
>   bağlantı **20 = k6'nın 20 VU'su**, sonra 0.
>
> **K8 ikinci ölçüm (Prometheus + Grafana):** `available` 2127 Mi, request'ler %37, OOMKilled yok,
> disk %24. Grafana working set 266 Mi: anonim 95 Mi, 199 Mi sayfa önbelleği (binary ve veritabanı;
> kernel OOM'dan önce geri alır), Go heap 71 Mi. K8'in son ölçümü birkaç saatlik çalışmadan sonra.
>
> **G6 için not:** Grafana'nın `/metrics`'i girişsiz 200 dönüyor; Traefik sadece public dashboard
> yollarını (ve statik dosyaları) geçirmeli. Ölçümde bir kez prod'da onaysız `kubectl exec`
> (cgroup `memory.stat` okuma, salt okuma) çalıştırıldı; kural ihlali olarak kayıtlı.

### G6. Herkese açık salt okunur dashboard: `status.gamemesh.app` (danışman madde 4)

- [x] **Ne:** 4 paneli girişsiz ve düzenlenemez biçimde dışarı açmak; admin paneli ve
  Prometheus açılmadan.
- **Seçenekler (karar):**
  | Seçenek | Nasıl | Risk |
  |---|---|---|
  | (a) Ayrı, **salt okunur bir Grafana** | Anonim erişim açık, rol Viewer, login formu kapalı, sadece provisioning'den gelen dashboard; admin işleri için ayrı (tailnet'ten) Grafana | İki Grafana; ama dışarıdakinin hiç admin kullanıcısı yok |
  | (b) Tek Grafana, anonim Viewer + Traefik'te yol filtresi | `/login`, `/admin`, `/api/admin`, `/api/dashboards/db` (yazma) gibi yollar Traefik'te kapatılır | Grafana'nın yol yüzeyi geniş; filtre eksik kalabilir |
  | (c) Grafana'nın "public/shared dashboards" özelliği | Tek bir dashboard'a özel, girişsiz bir URL | Grafana 11.6 OSS'teki durumu ve kök adresin nasıl görüneceği **doğrulanacak** |
  Öneri: (a) ya da (c), doğrulamaya göre; (b) en zayıfı.
- **Diğer işler:**
  - **DNS:** `status` için A kaydı (Cloudflare, DNS-only), CAA zaten Let's Encrypt. Kayıt dışa
    açık bir işlem → onay.
  - **TLS:** cert-manager, önce `letsencrypt-staging`.
  - **Admission policy:** bugün sadece `gamemesh` namespace'inde, sadece `api.gamemesh.app`.
    `monitoring` namespace'inde yalnızca `status.gamemesh.app`'e izin veren bir binding ya da
    parametre; `gamemesh`'teki kural değişmez.
  - **Yük:** Traefik rate limit middleware'i, panellerde en kısa yenileme aralığının sınırı
    (Grafana `min_refresh_interval`), sorgu zaman aşımı.
  - **Ingress kökü:** `/` doğrudan dashboard'a gitsin.
- **Bitti sayılır:** K13 (ve K6): anonim bir tarayıcı dashboard'u görüyor; bir kaydetme denemesi
  (`POST /api/dashboards/db`) ve admin API'leri (`/api/admin/...`, `/api/users`) reddediliyor;
  `/login` ya kapalı ya işe yaramıyor; Prometheus'a dışarıdan ulaşılamıyor.

> **Sonuç (2026-10-24): bitti (PR #99, #100; prod'da `6b12caf`).** `https://status.gamemesh.app`
> girişsiz ve salt okunur; Grafana'nın paylaşılan (public) dashboard'u, Grafana'da anonim kullanıcı
> hiç yok. Seçenek (c), üç katman:
>
> 1. **Grafana:** sadece paylaşımın token'lı yolları girişsiz çalışıyor.
> 2. **Traefik** (`deployments/k8s/monitoring/status.yaml`): sadece sayfanın yüklediği yollar
>    (headless Chromium ile ölçüldü: 58 istek, üç önek): `/public-dashboards/<token>`,
>    `/api/public/dashboards/<token>`, `/public/`; `/` sayfaya 302; HTTP→HTTPS; istemci başına
>    10 istek/sn, burst 100 (ilk yükleme ~47 istek, sonra 10 sn'de 5).
> 3. **Admission policy** (cluster seviyesi): `monitoring`'de Ingress sadece `status.gamemesh.app`,
>    bu önekler ve `grafana` Service'i (ya da cert-manager'ın ACME challenge'ı, kendi solver'ına).
>
> **Kararlar (onaylandı):** paylaşımı `publisher` sidecar'ı kuruyor ve koruyor (Grafana'nın kendi
> imajı; `grafana/publish.sh`): yoksa sabit token'la oluşturuyor, kapatılırsa ya da ayarları
> değişirse geri alıyor, elle başka token'la değiştirilirse silip yeniden kuruyor. Token sayfanın
> URL'si, bilerek repoda; dört yerde yazılı ve CI tutarlılığını kontrol ediyor
> (`scripts/k8s/check-status-token.sh`). İzleyici zaman aralığını değiştiremiyor, annotation yok.
> `root_url` public adres. TLS önce staging (deploy'dan sonra 1 dk'dan kısa; HTTP-01 solver'ın
> Ingress'i policy'den geçti), sonra prod: Let's Encrypt YE1, ECDSA P-256, 2027-01-22'ye kadar.
> DNS: `status` A kaydı, `api` ile aynı adres, DNS-only, AAAA yok (kullanıcı ekledi).
>
> **Kanıt (internetten, gerçek sertifikayla):**
> - **Anonim tarayıcı:** headless Chromium `https://status.gamemesh.app/` → 302 → sayfa; 53 istek,
>   hepsi bu host'ta ve üç önekte, 302 dışında hepsi 200; paneller prod verisini gösteriyor
>   (1,5 req/s, p95/p99 2,5–5 ms, %0 hata, 0 bağlantı); ekran görüntüsü alındı.
> - **Kapalı (Traefik'in 404'ü, Grafana'ya ulaşmıyor; 23 yol):** `/login` (GET, POST), `/logout`,
>   `/metrics`, `/api/health`, `/api/search`, `/api/datasources` ve Prometheus proxy'si,
>   `/api/admin/settings`, `/api/users`, `/api/org`, `/api/dashboards/…`,
>   `POST /api/dashboards/db` (kaydetme), `POST /api/ds/query`, başka bir paylaşımın sayfası ve
>   API'si, `/d/…`, `/apis/`, `/explore`, Prometheus'un `/api/v1/query`, `/-/ready`, `/graph`'ı.
> - **İzinli önekte değiştirme:** `DELETE`/`PATCH /api/public/dashboards/<token>` Traefik'ten
>   geçiyor (Ingress method ayırmıyor), Grafana 401 ile reddediyor.
> - **Kendi sorgusu:** panel sorgusunun gövdesi `up` ya da ham sayaçları istese de kayıtlı sorgunun
>   tek serisi dönüyor (`up` 15 seri); olmayan panel veri döndürmüyor.
> - **Rate limit:** tek IP'den 250 hızlı istek → 117 × 200, 133 × 429; 12 sn sonra 200.
> - **Politika (prod, `--dry-run=server`):** izinli 3 durum kabul, yasak 5 durum (`/login`,
>   `/metrics`, `/` Prefix, Prometheus'a yönlendirme, başka host) ret; yerelde 5/9.
> - **K6:** Grafana'nın admin girişi dışarıdan yok (`/login` 404, 3000 kapalı), sadece port-forward
>   (tailnet); Prometheus dışarıdan erişilemiyor.
> - **Yerel:** publisher 4 senaryoda (yokken, kararlı, elle kapatılmış, başka token) her birini bir
>   turda düzeltti; admin girişi `root_url` ayarlıyken port-forward'la çalışıyor.
>
> **Not:** Grafana arayüzündeki paylaşım linkleri artık public adresi gösteriyor (`appUrl`); admin
> işini bozmuyor. Yerel ölçümde test token'ı bir dosyada bir süre maskelenmeden kaldı (silinmiş
> yerel bir paylaşıma ait, scratchpad'de).

### G7. k6 ile doğrulama (danışman madde 5)

- [x] **Ne:** Prod'a ölçülü bir yük basıp panellerin gerçek zamanlı hareket ettiğini görmek,
  ekran görüntüsü almak.
- **Dikkat:**
  - **Rate limit:** tek IP'den 50 rps'in altında kalmak (ör. 30–40 rps) ya da yükü birden fazla
    kaynaktan (ör. GitHub Actions runner'ları) vermek. Limiti prod'da yükseltmek yok.
  - **Ev ağı (#83):** k6 evden koşarsa ağ dalgalanması panellere yansır; tercihen bir CI
    runner'ından, ya da ölçüm sırasında ağın durumu da kaydedilir.
  - **Prod verisi:** kayıt/login senaryoları prod veritabanına kullanıcı yazar; okuma ağırlıklı
    senaryolar (`/api/v1/leaderboard`, `/health`) ve az sayıda, işaretli test hesabı; sonra
    temizlik.
  - **WebSocket:** aktif bağlantı panelinin hareket etmesi için k6'nın WebSocket senaryosu.
  - **Bulgu #91:** k6'nın p95'i setup'taki bcrypt isteklerini de içeriyor; senaryonun kendi p95'i
    (`http_req_duration{scenario:...}`) ayrı raporlanır ve README'deki tablo buna göre düzeltilir.
- **Bitti sayılır:** K14: yük sırasında 4 panelin ekran görüntüsü; panel değerleri k6'nın
  raporuyla (req/s, p95, hata, bağlantı sayısı) karşılaştırılıp tutarlılığı yazıldı.

> **Sonuç (2026-10-25, K14): bitti.** `scripts/k6/status-demo.js` (prod'a güvenli: okumalar ve
> WebSocket'ler, 20 işaretli hesap `k6g7…`, skor/kuyruk/oda yok; mevcut betikler yüzlerce hesap açıyor
> ve herkese açık skor tablosuna yazıyor). Evden, 12:08:51–12:14:23 UTC, 1 dk rampa + 3 dk sabit + 1 dk
> iniş; ağ önce 52 ms / %0, sonra 68 ms / %0.
>
> | Panel (12:10:20–12:13:20, public API'den) | Panel | k6 | |
> |---|---|---|---|
> | İstek hızı | ort. 28,7, en çok 30/s | hedef 30/s (okumalar 7199) | ✓ ±%10 |
> | Hata oranı | %0 | %0 (0/7199) | ✓ |
> | Aktif bağlantı | 18–20 (ort. 19,6) | 20 VU, 64 açılış %100 | ✓ (bağlantılar 90 sn'de bir yenileniyor) |
> | Gecikme | p95 2,5 ms, p99 4,5 ms (sunucu) | p95 57,5 ms, medyan 46,9 ms (istemci) | ✓ fark ~55 ms ≈ RTT |
>
> Ekran görüntüsü yükün ortasında (12:11 UTC) alındı: `docs/images/status-page-under-load.png`,
> README'de. Gecikme panelinin başındaki 5 s'lik sıçrama gerçek: k6'nın setup'ı 20 register + 20 login'i
> aynı anda gönderiyor, bcrypt 2 vCPU'da sıraya giriyor (en çok 4,9 s) ve o dakikada tek trafik onlar
> (#91'in prod'daki karşılığı). Logaritmik y ekseni ikisini birden gösterirdi; görsel cila, roadmap.
>
> **Notlar:** İlk koşu geçersiz: Mac pildeyken kapağı kapanınca uykuya geçti (pmset: Clamshell Sleep,
> 22:58:29 UTC; k6 1 sa 11 dk sürdü); ikincisi `caffeinate` ve şarjla. Test hesapları kullanıcının
> çalıştırdığı transaction'la silindi (40 → 0, sahipsiz `player_stats` 0; prod'da `exec`'i otomatik izin
> kontrolü engellemişti). **#91:** k6 betikleri artık `http_req_duration{phase:load}`'u raporluyor;
> yerelde matchmaking (500 VU, 1 dk) iki koşuda yükün p95'i 21,5 ve 26,6 ms, bütün isteklerin 111,9 ve
> 182,5 ms. README'nin benchmark tablosu #96'nın düzeltmesinden sonra yeniden ölçülecek: leaderboard
> satırı (133/s, %0) güncel kodla yeniden üretilemiyor.

### G8. Uyarı kuralları ve yönlendirme — *Faz 3'e taşındı*

**Karar (2026-10-22):** Danışmanın kapsamına uyularak Faz 3'e. Faz 3 için not edilenler:
Alertmanager kuralları (pod hazır değil/crashloop, disk > %80, bellek baskısı ve OOMKilled,
scrape başarısız; sertifika süresi ve yedek yaşı Faz 2'de F1/F2'de zaten var), yönlendirme F1'deki
kanala, `promtool test rules` ve gerçek bir tetikleme. Faz 2'de G2'deki yığın seçilirken
Alertmanager kapalı kalır (kube-prometheus-stack seçilirse `alertmanager.enabled: false`).

### G9. Runbook ve README roadmap

- [ ] **Ne:**
  - `docs/deployment.md`: "Restoring from backup", "Monitoring access", "Status page",
    "When an outside alert fires" (F1, F2).
  - README: "Roadmap" bölümü (danışman: koda değil): Alertmanager ve alarm kuralları (Faz 3),
    log toplama (Loki), uzun süreli metrik saklama, tema ve görsel cila, ek
    paneller; Faz 1 sonrası listesinden öne çıkanlar.
- **Bitti sayılır:** K9, K15; E5 tatbikatı bu bölümlerle yapıldı.

> **Ara sonuç (2026-10-25): K15 bitti, K9 E ve F'yi bekliyor.** README'de "Roadmap" bölümü
> (danışmanın kapsam dışı listesi, bu fazda not edilenler, Faz 1 sonrası listesinden öne çıkanlar;
> henüz yapılmamış yedek ve dış izleme "devam ediyor" olarak). `docs/deployment.md`'de "Monitoring" ve
> "Status page" bölümleri G4–G6'da yazıldı; "Restoring from backup" ve "When an outside alert fires"
> E ve F'yle birlikte gelecek, kutu onlarla işaretlenecek.

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
  Özel anahtar sadece admin'de (parola yöneticisi + çevrimdışı kopya); sunucuda **yok**.
- **Erişim anahtarı:** depoya sadece bu bucket'a yazabilen bir token; tercihen silme yetkisi
  olmadan (object lock / retention destekleniyorsa). Desteği **doğrulanacak**.
- **Saklama:** örn. 7 günlük + 4 haftalık + 3 aylık; depo yaşam döngüsü kuralıyla.
- **Öğrenilecek:** RPO/RTO, mantıksal ve fiziksel yedek farkı, 3-2-1 kuralı, istemci tarafı
  şifreleme, değiştirilemez yedek.
- **Bitti sayılır:** Kararlar plana yazıldı ve onaylandı.

### E2. Postgres yedek CronJob'u

- [ ] **Ne:** `gamemesh` namespace'inde günlük bir CronJob: `pg_dump --format=custom` →
  `age` ile şifrele → uzak depoya yükle → başarıda dead man's switch'e ping (F2).
- **Nasıl (taslak):**
  - Postgres'le **aynı sürümde** bir imaj (`postgres:16.x-alpine`, digest ile) ve yükleme
    için küçük bir araç (`rclone` ya da `aws-cli`), digest ile.
  - PSS `restricted` altında: non-root, read-only root filesystem, geçici veri `emptyDir`'de.
  - Depo erişim anahtarı ve `age` **public** key'i bir Secret'ta (`gamemesh-backup`), admin
    oluşturur; CI'ın Secret yetkisi yok.
  - `concurrencyPolicy: Forbid`, `startingDeadlineSeconds`, `activeDeadlineSeconds`; 03:30
    UTC'deki otomatik reboot penceresiyle çakışmasın (ör. 02:15 UTC).
  - CronJob prod overlay'inde mi (CI'ın Role'üne `batch/cronjobs` eklenir) yoksa cluster
    seviyesinde mi; karar (en az yetki).
- **Bitti sayılır:** Elle tetiklenen bir job uzak depoya şifreli bir dosya bıraktı; ertesi gün
  kendiliğinden bir tane daha geldi (K1).

### E3. k3s durumunun yedeği (#80)

- [ ] **Ne:** k3s datastore'u (SQLite) ve `server/cred/` + `server/token` için yedek; ikisi
  **ayrı** yerlerde.
- **Neden:** Datastore'da bütün Kubernetes nesneleri var (Secret'lar şifreli); anahtar
  `cred/`'de. İkisi birlikte sızarsa bütün Secret'lar açılır. Ayrıca bu yedek olmadan bir
  yeniden kurulumda Secret'lar yeniden üretilir ve Postgres parolası volume'dakiyle uyuşmaz.
- **Nasıl (taslak):** host üzerinde bir systemd timer: SQLite'ın tutarlı kopyası (yöntem
  **doğrulanacak**), şifrele, yükle; `cred/` için ayrı anahtar/depo ya da çevrimdışı.
- **Bitti sayılır:** E5'te geri yükleme denendi (K4).

### E4. Yedek izleme

- [ ] **Ne:** Başarılı her yedek bir dead man's switch'e (F2) ping atar; ping gelmezse uyarı.
- **Bitti sayılır:** Job bilerek bozuldu; uyarı geldi, süresi ölçüldü (K3).

### E5. Geri yükleme tatbikatı

- [ ] **Ne:** Boş bir Ubuntu VM'de (Faz 1 D4'teki Multipass yöntemi) runbook'u izleyerek
  cluster kurmak, k3s durumunu ve Postgres yedeğini geri yüklemek, geri yüklenen bir hesapla
  giriş yapmak.
- **Ölçülecek:** RTO, RPO, adım adım süre, runbook'taki eksikler.
- **Dikkat:** Tatbikat cluster'ına `letsencrypt-prod` kurulmaz (Faz 1 D4 bulgusu).
- **Bitti sayılır:** K2 ve K4.

## Bölüm F — Dış izleme ve uyarı

### F1. Dış izleme servisi ve uyarı kanalı

- [ ] **Ne:** Sunucunun ve ev ağının **dışında** çalışan bir izleme: `https://api.gamemesh.app/health`
  (200 ve `"status":"ok"`), bir proxy'lenen route (`/api/v1/leaderboard`), TLS sertifikasının
  kalan süresi; G6'dan sonra `status.gamemesh.app` de.
- **Seçenekler:** UptimeRobot, Better Stack, başka yerde barındırılan Uptime Kuma, zamanlanmış bir
  GitHub Actions workflow'u (kısa aralıklar için güvenilmez). Ücretsiz katman ve özellikler
  **doğrulanacak**.
- **Uyarı kanalı:** e-posta (`ops@gamemesh.app`) ve bir mobil bildirim; e-postayı servisin
  kendisi gönderir (domain'den gönderim yok).
- **Bitti sayılır:** K5: gateway bilerek 0 replikaya indirildi (onaylı); uyarının ulaşma süresi
  ölçüldü; "düzeldi" bildirimi geldi.

### F2. Dead man's switch

- [ ] **Ne:** Yedek job'larının (E2, E3) "çalıştım" ping'i için bir servis (ör.
  healthchecks.io); beklenen aralıkta ping gelmezse F1'deki kanala uyarı.
- **Bitti sayılır:** E4.

### F3. Kök adres (isteğe bağlı)

- [ ] **Ne:** `api.gamemesh.app/` bugün 404; CV'deki link bu yüzden `/health`. İsteğe bağlı:
  kökte küçük bir karşılama cevabı (ad, durum, sürüm, repo ve durum sayfası linki) ya da
  `status.gamemesh.app`'e yönlendirme; Ingress'e `/` için `Exact` path (admission policy aynı
  host'a izin veriyor).

## Bölüm H — Tarihli bakım (Faz 1'den)

| Tarih | İş | Bulgu |
|---|---|---|
| 2026-11-04 | `nats-server` CVE-2026-111943 kaydı bitiyor: yamalı NATS (Dependabot) gelirse güncelle ve kaydı sil; gelmezse yeniden değerlendir | #88 |
| 2026-11-18 | Redis imajındaki OpenSSL CVE-2026-84782 kaydı bitiyor | — |
| 2026-12-20 | `api-tls`'in ilk otomatik yenilemesi (admission policy altında): `kubectl -n gamemesh get certificate` ile doğrula; F1'deki sertifika kontrolü de görmeli | #85, #87 |
| 2027-01-18 | gosu Go CVE kayıtları bitiyor | — |
| 2027-04-21 | KSV-0056 kaydı bitiyor | #87 |

## Kapsam dışı

**Danışmanın kapsam dışı listesi** (README'de "Roadmap" olarak, koda değil; G9): Alertmanager ve
alarm kuralları (Faz 3; G8 ve K7 buraya taşındı), log toplama (Loki), uzun süreli saklama, tema
ve görsel cila, ek paneller.

**Faz 3 ya da sonrası:** Alertmanager ve uyarı kuralları (G8, K7); yüksek erişilebilirlik (birden
fazla node, yönetilen veritabanı); arm64 imaj taraması (#78), cert-manager imajlarının taranması (#84), `secretbox` (#80), JWT `kid`
rotasyonu, k3s 1.36, dual-stack (#81), matchmaking için leader lock (Faz 1 planının "Faz 1
sonrası" listesi). Faz 2'nin bir adımı bunlara dokunuyorsa birlikte ele alınır.

## Bulgu dizini

Faz 1'in dizini 89'da bitti; Faz 2'nin bulguları **#90**'dan başlar.

| # | Bulgu | Kaynak | Kanıt | Adım |
|---|---|---|---|---|
| 90 | HTTP metriklerinin `method` label'ı sınırsız: her uydurma method yeni bir zaman serisi açıyor (gateway belleği, ileride Prometheus); Traefik bunu internetten gateway'e iletiyor | G (danışmanın kardinalite notu) | Ölçüldü (yerel: 25 uydurma method → 25 seri; prod: `ZZCHECK1` gateway log'unda, 404) | G3, **düzeltildi** (PR #91; prod'da 10 uydurma method → `other`, seri sayısı sabit) |
| 91 | README'deki "112 ms p95" bir kenar etkisi: k6'nın p95'i setup'taki 1000 bcrypt isteğini (~205 ms, toplamın tam %5'i) de içeriyor ve p95 iki tepenin arasına düşüyor | G3 ölçümü | Ölçüldü (aynı iki koşuda toplam p95 113 ve 187 ms; yük aşamasında 27,7 ms, setup 211 ms) | G7, **düzeltildi** (PR #102: `phase: load` etiketi; README tablosu #96'nın PR'ında yeniden ölçüldü: matchmaking yük p95'i 27–31 ms, bütün istekler 86–190 ms) |
| 92 | WebSocket oturumlarının ömrü HTTP gecikme histogramına giriyor (gateway'de handler oturum bitene kadar dönmüyor); el sıkışma 101 yerine 200 sayılıyor | G3 ölçümü | Ölçüldü (yerel: 27 oturum `+Inf`'te, toplam 1151 s) | G3, **düzeltildi** (PR #91; hijack → `101`, histogramda yok) |
| 93 | k3s'te kubelet'in `/metrics`'i aynı süreçteki kontrol düzleminin metriklerini de veriyor; kube-prometheus-stack'in varsayılanları kontrol düzlemini iki kez scrape ediyor | G1 ölçümü (yerel k3d) | Ölçüldü (`job="kubelet"` `/metrics`: 37.346 `apiserver_*` + 12.966 etcd/workqueue/scheduler, 947 `kubelet_*`) | G2 (sade yığın seçildi; kube-prometheus-stack'e dönülürse `kubelet` job'unda `/metrics` filtrelenir) |
| 94 | Planlanan monitoring sürümlerinde CI'ı kıracak bulgular; temiz bir Grafana sürümü yok (hepsi Go'nun CVE-2026-111943'üyle derlenmiş) | G4 (yükseltme öncesi tarama) | Ölçüldü (Trivy 0.76.1, CI kuralları: Prometheus 3.13.5 → 2, 3.15.1 → 0; Grafana 13.2.4 → 75, 13.1.8 → 18, 12.4.13 → 8) | G4 (Prometheus 3.15.1); G5 (Grafana sürümü) |
| 95 | CI'da bir build'in GitHub Actions cache'ine aktarımı 17 dk 40 sn takıldı (job sınırına 81 sn kala); `type=gha`'nın 10 dk varsayılan timeout'u devreye girmedi. Repo cache'i 10,17 GiB ile sınırın üstündeydi, 3,68 GiB'ı kapanmış PR'lardan | PR #95'in CI'ı | Ölçüldü (job log'u: `sending cache export 1060.2s`; aynı koşudaki diğer 6 servis 1,7–6,9 sn) | G4 (cache import/export 3 dk timeout + `ignore-error`; kapanan PR'ın cache'ini silen workflow; tek seferlik temizlik → 7,05 GB) |
| 96 | Gateway upstream bağlantılarını eşzamanlı yükte yeniden kullanmıyor: `http.DefaultTransport` upstream başına 2 boşta bağlantı tutuyor, gerisi her yanıttan sonra kapanıp `TIME_WAIT`'e düşüyor ve yerel portlar tükeniyor; aynı varsayılan presence notifier'da | G7 (README benchmark'ının yeniden ölçümü) | Ölçüldü (yerel, leaderboard 500 VU, ~770 istek/sn: 9941 × 502, %12,7; hepsi `dial tcp …:8083: connect: cannot assign requested address`) | **Düzeltildi** (`fix/gateway-upstream-pool`): gateway'e kendi Transport'u (upstream başına 256 boşta bağlantı), notifier'a gövdeyi okuma + 64 bağlantı. Test: 1000 istek 594 → 50 bağlantı; notifier 500 → 20. Yük altında: `TIME_WAIT` 28.396 → 967 (port aralığı 28.232), hata %5,7 → %0, 782 → 3374 istek/sn |
| 97 | İstemcinin iptal ettiği istekler (`context canceled`) gateway'de 502 olarak yanıtlanıyor ve sayılıyor: hata oranı paneli (5xx) kullanıcının vazgeçtiği istekleri sunucu hatası gösteriyor | #96'nın yeniden ölçümü | Ölçüldü (yerel, leaderboard koşusunun son dakikası: 436 × 502, 424'ü `context canceled`, 12'si `EOF`, hepsi k6 süre dolunca iptal ettiğinde) | **Düzeltildi** (`fix/gateway-upstream-pool`): iptalde 499 (nginx'in "client closed request"'i), 5xx sayılmıyor; test 502 → 499, kapalı upstream hâlâ 502 |
