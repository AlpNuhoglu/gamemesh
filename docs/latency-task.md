# GameMesh — Oyun içi gecikmeyi ölç, azalt, belgele
 
## 0. Önce oku: delegasyon politikası
 
Bu göreve başlamadan önce `.claude/skills/fable-orchestration/SKILL.md`
dosyasını oku ve içindeki yönlendirme politikasına uy.
 
Politikanın bu görevdeki karşılığı:
 
- Ana döngü planı, brief'leri ve karar gerektiren kısımları yazar; işin kendisi
  uygun katmana yönlendirilir.
- Her alt-ajan çağrısında model **açıkça** belirtilir. Model yazılmayan çağrı,
  en pahalı katmanı sessizce çoğaltır.
- Zorluğa göre yönlendir, sonra işi taşıyabilecek en ucuz katmanı seç. İki
  katman arasında kararsızsan ucuz olanı seç ve sonucu doğrula; başarısız
  olursa bir katman yukarı çık.
- Alt-ajanın raporu bir iddiadır. Diff'i, çıktıyı veya bir örneği kontrol
  etmeden "tamam" diye raporlama.
- Ana döngünün bağlamını temiz tut: geniş dosya okumaları ve ham veri bir
  delegeye gider, ana döngü yalnızca karar vermesi gerekeni okur. Küçük işleri
  (birkaç tool çağrısı, tek bir bilinen dosya) kendin yap; orada delegasyonun
  maliyeti kazancından fazladır.
Bu görevdeki işlerin tahmini dağılımı (nihai karar senin):
 
| İş | Nitelik |
|---|---|
| Gecikme yolunun mimarisini çıkarmak, B1'in sınırını belirlemek | Karar işi — ana döngü |
| `cmd/latencybench` aracını yazmak, aşama zaman damgalarını eklemek | Orta-yüksek: çok dosyalı implementasyon |
| B2–B5 gibi kapsamı dar, tek noktalı değişiklikler | Basit-orta: adı belli dosyada sınırlı düzenleme |
| Ölçüm çıktılarını tabloya dökmek, `docs/latency.md`'yi biçimlendirmek | Toplu/mekanik iş |
 
**B1 (geçici veriyi dayanıklı yoldan ayırma) bu görevin en riskli kısmı.**
Yanlış yapılırsa maç sonucu ve skor gibi kaybolmaması gereken olaylar
garantisiz yola düşer. Bu maddeyi düşük katmana devretme; devredersen diff'i
satır satır kontrol et.
 
## 1. Bağlam
 
GameMesh, Go ile yazılmış gerçek zamanlı çok oyunculu oyun backend'i. Yedi
servis: gateway, player, matchmaking, leaderboard, presence, websocket,
outbox-relay. Altında PostgreSQL, Redis ve NATS JetStream var. Canlıda:
`api.gamemesh.app`, tek node k3s.
 
Elimizde 112 ms p95 ölçümü var, ama bu **HTTP isteklerinin yük altındaki
p95'i**. Oyun için önemli olan sayı bu değil: bir oyuncunun hamlesinin
WebSocket üzerinden diğer oyuncuya ulaşma süresi. Bu sayı şu an bilinmiyor.
 
## 2. Hedef
 
Oyuncudan oyuncuya mesaj gecikmesini ölçmek, sunucu tarafındaki
düşürülebilir kısmı düşürmek ve öncesi/sonrası farkını sayıyla belgelemek.
 
## 3. Faz A — Önce ölç (hiçbir değişiklik yapmadan)
 
### A1. Ölçüm aracı: `cmd/latencybench`
 
- İki WebSocket istemcisi açar, eşleştirir, birini gönderen diğerini alıcı
  yapar.
- Gönderen, mesaj gövdesine monotonik zaman damgası koyar. Wall clock
  kullanma; Go'da `time.Now()`'ın monotonik bileşeni üzerinden ölç.
- Alıcı mesajı aldığında geçen süreyi kaydeder.
- En az 500 örnek, 50 ms aralıkla. Isınma için ilk 50 örneği at.
- Çıktı: p50, p95, p99, max, kayıp mesaj sayısı.
- Aynı aracı **1, 10 ve 50 eşzamanlı çift** için çalıştırabilmeli.
### A2. Gecikmeyi parçalara ayır
 
Mesaj yolundaki her aşamaya zaman damgası koy ve farkı ölç:
 
```
istemci gönderdi → sunucu okudu → sunucu işledi → sunucu yazdı → istemci aldı
```
 
Sunucu içi süreyi Prometheus histogramı olarak da yayınla (örn.
`gamemesh_ws_relay_duration_seconds`). Hangi aşamanın kaç ms aldığı net
görünmeli.
 
### A3. Taban ölçümünü kaydet
 
`docs/latency.md` dosyası oluştur. Ölçüm ortamı (nereden, hangi sunucuya, kaç
çift), ham sayılar ve aşama dökümü yazılsın. Bu dosya her değişiklikten sonra
güncellenecek.
 
## 4. Faz B — Değişiklikler
 
**Her maddeyi ayrı commit olarak yap ve her maddeden sonra `latencybench`'i
tekrar çalıştır.** Hangi değişikliğin kaç ms kazandırdığı tek tek görünmeli.
Hepsini birden uygulayıp tek ölçüm alma.
 
### B1. Geçici veriyi dayanıklı yoldan ayır — en yüksek öncelik
 
Hareket, konum ve diğer oyun içi mesajlar NATS JetStream ve outbox üzerinden
**geçmemeli**. Bunlar geçici veridir: kaybolursa bir sonraki paket düzeltir.
Doğrudan WebSocket hub'ından yayınlanmalı.
 
Outbox ve JetStream yalnızca kaybolmaması gereken olaylar için kalacak: maç
sonucu, skor, sıralama değişimi, oyuncu durumu.
 
Bu ayrımı kodda açık bir sınır olarak kur (örn. ayrı kanal veya mesaj tipi),
sonradan karışmasın.
 
### B2. Nagle'ı kapat
 
WebSocket bağlantılarının altındaki TCP soketinde `SetNoDelay(true)`. Küçük
paketlerde 40 ms'e varan gecikme ekleyebilir.
 
### B3. Sıkıştırmayı kapat
 
`permessage-deflate` oyun kanalında kapalı olsun. Küçük mesajlarda faydasız,
CPU ve gecikme ekliyor.
 
### B4. Batching'i oyun kanalından çıkar
 
Mevcut in-memory batching, datastore round-trip'lerini azaltmak için var; oyun
mesajlarına uygulanmamalı. Uygulanıyorsa kaldır veya bu kanal için 16 ms'e
indir.
 
### B5. Gereksiz atlamaları kaldır
 
Oyun WebSocket trafiği gateway üzerinden ek bir atlama yapıyorsa doğrudan
websocket servisine bağlansın.
 
### B6. Yazma yolunu incele
 
Her bağlantı için write pump'ta yapay ticker veya bekleme var mı, write buffer
boyutu, flush davranışı. Mesaj hazır olduğunda beklemeden yazılmalı.
 
## 5. Kısıtlar
 
- Yeni mikroservis **ekleme**.
- Sunucu taraflı oyun döngüsü veya otoriter simülasyon **ekleme**. Sunucu
  mesajı aktarır ve ayrık kararları verir; fizik simüle etmez.
- Mimariyi yeniden yazma. Değişiklikler mümkün olan en küçük dokunuşlar olsun.
- Outbox ve JetStream'i dayanıklı olaylardan **kaldırma**. Sadece geçici veriyi
  oradan çıkar.
- Mevcut testler geçmeye devam etmeli. `go test -race` temiz olmalı.
- Ölçüm yapmadan "bu daha hızlıdır" deme. Her iddia sayıyla gelsin.
- Bir değişiklik ölçümde kazanç göstermiyorsa **geri al** ve bunu
  `docs/latency.md`'ye yaz. Çalışmayan değişiklik kodda kalmasın.
## 6. Teslim edilecekler
 
1. `cmd/latencybench` — tekrar çalıştırılabilir ölçüm aracı
2. `docs/latency.md` — ortam, taban ölçüm, her değişiklik sonrası ölçüm, aşama
   dökümü, geri alınan değişiklikler
3. Her değişiklik için ayrı commit, mesajında kazanılan ms ile
4. Prometheus histogramı: sunucu içi aktarma süresi
5. README'ye kısa bölüm: "Oyun içi gecikme" — öncesi/sonrası tablosu
6. **Delegasyon kaydı:** hangi işi hangi modele verdin, neden o katmanı
   seçtin, ve her delegenin çıktısını nasıl doğruladın. Doğrulanmadan kabul
   edilen çıktı varsa belirt.
## 7. Rapor
 
İş bittiğinde şunu yaz:
 
- Taban ve son ölçüm tablosu (p50/p95/p99, 1 ve 50 çift için)
- Her değişikliğin tek tek kazancı, ms olarak
- Kalan gecikmenin dökümü: ne kadarı ağ (düşürülemez), ne kadarı sunucu
- Denenip geri alınanlar ve neden
- Önerdiğin ama yapmadığın şeyler (kapsam dışı kalanlar)
 