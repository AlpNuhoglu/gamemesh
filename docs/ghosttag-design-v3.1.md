# Ghost Tag — Oyun ve Teknik Tasarım Dokümanı

**Sürüm:** v3.1 (kesinleşmiş spesifikasyon; 2026-11-16 uygulama kararları
işlendi, bkz. bölüm 13)
**Proje:** GameMesh — Faz 3 demo istemcisi

---

## 0. Özet

Ghost Tag, GameMesh backend'i üzerinde çalışan iki kişilik, asimetrik bir
kovalamaca oyunudur. Dairesel bir arenada bir oyuncu **Avcı**, diğeri
**Kaçak** rolündedir. Avcı kaçağın canlı konumunu görür; kaçak avcıyı
yalnızca 2 saniye geriden gelen sönümlenen ışık izi olarak görür. Maç iki
yarıdan oluşur ve yarı arasında roller değişir.

Tasarımın merkezindeki fikir: **ağ gecikmesi gizlenmez, oyunlaştırılır.**
Ölçülen tek yön gecikmesi ~40 ms iken kaçağın gördüğü iz 2000 ms geriden
gelir; ağ gecikmesi bu tasarımda görünmez hale gelir.

---

## 1. Çekirdek Mekanikler ve Sunucu Doğrulama Modeli

### 1.1 Saat Otoritesi ve Paket Sıralama Kuralı

**Tek zaman otoritesi (`ServerTs`).** Hareket doğrulaması, halka tampon
hizalaması, yakalama penceresi, arena daralması `R(t)` ve beraberlik bozucu
örneklemesi yalnızca paketin sunucuya ulaştığı anki sunucu saatini kullanır.

**İstemci zaman damgası (`cts` / `seq`).** Oyun kurallarında ve sunucu
kararlarında asla kullanılmaz; saldırgan kontrolündedir. Yalnızca iki amaca
hizmet eder:

- Ağda sırası bozulmuş veya yinelenmiş paketleri elemek (`seq <= last_seq`
  ise yoksay)
- İstemci tarafı enterpolasyon tamponunu düzenlemek

### 1.2 Hareket Doğrulaması, Dönüş Ataleti ve Durum Sıfırlama

İstemciden gelen `vx`, `vy` alanları yalnızca diğer istemcilerin
dead-reckoning tahmini için taşınır. **Sunucu yön ve hız hesabını yalnızca
ardışık iki doğrulanmış konumdan türetir.**

#### Başlangıç ve sıfırlama kuralı (`has_prev_motion == false`)

**Yarı başlangıcında** (`COUNTDOWN_1` ve `ROLE_SWAP` sonrası): her iki oyuncu
simetrik başlangıç noktalarına yerleştirilir; `θ_{n-1}`, `ServerTs` ve halka
tamponlar temizlenir. Yeni yarının ilk hareket paketinde konum, başlangıç
noktasına göre hız sınırından geçirilir ancak **açısal atalet kontrolü
atlanır** ve ilk yön `θ_0` kaydedilir.

**`DISCONNECT_GRACE` dönüşünde:** oyuncunun kopma anındaki son doğrulanmış
konumu (`P_anchor`) korunur, ancak `ServerTs` ve `θ_{n-1}` sıfırlanır. Dönüş
sonrası ilk paket, 5 saniyelik kopma süresinin `Δt_max` kırpmasına takılıp
oyuncuyu kilitlememesi için yalnızca `P_anchor` üzerinden kurulur.

> **Uygulama notu:** `has_prev_motion == false` durumunda `dt` olarak doğrudan
> `Δt_max` (0,25 sn) kullanmak, yarı başında tek adımda ~90 birimlik sıçramaya
> izin verir. Spawn sonrası ilk pakette tek tick süresi kullanılır:
> `Δt_initial = 0,04 sn`.

#### Ardışık paket doğrulama adımları (`has_prev_motion == true`)

**1 — Zaman adımı sınırlandırma**

```
Δt = clamp(ServerTs_n − ServerTs_{n-1}, 0, Δt_max)     Δt_max = 0,25 sn
```

`Δt` **gerçek geçen süredir**; yalnızca üstten kırpılır ve hem mesafe
bütçesinde (adım 3) hem açısal sınırda (adım 2) aynı değer kullanılır.
`Δt_max` paket birikmesiyle ışınlanmayı önler.

**`Δt` alttan kırpılmaz** (eski `Δt_min` = 0,02 sn tabanı kaldırıldı). Taban,
saniyede 50'den fazla paket gönderen istemciye beklemediği süre için bütçe
yazıyordu: 1 saniyedeki gerçek sürelerin toplamı en fazla 1 saniyedir, ama
tabanla kırpılmış sürelerin toplamı paket sayısıyla büyür. Mesafede bu
60 Hz'de %20 fazla bütçe demekti; dönüşte, ölçülen 1 saniyelik en büyük
kaçak dönüşü:

| Paket hızı | Tabanla | Tabansız | Nominal `ω_max` |
|---|---|---|---|
| 25 Hz | 4,20 rad | 4,20 rad | 4,2 |
| 60 Hz | 5,04 rad | 4,20 rad | 4,2 |
| 120 Hz | 10,08 rad | 4,20 rad | 4,2 |

Dönüş için ayrı bir kredi havuzu yoktur: gerçek sürelerin toplamı zaten
sınırlı olduğu için 1 saniyede en fazla `ω_max` radyan dönülür, paket
hızından bağımsız. Bedeli bölüm 11'de: kümelenmiş paketlerde dönüş
kırpılabilir.

**Mikro hareket.** `‖d_ham‖ < ε_hareket` (10⁻³ birim) ise paket "duruyor"
sayılır: konum ve `θ` güncellenmez, yön türetilmez. Milimetre altı bir yer
değiştirmenin açısı gürültüdür; ondan yön türetmek kaçağın `θ`'sını rastgele
savurur ve sonraki gerçek adımda haksız açısal kırpmaya yol açar.

**2 — Yön türetme ve rol bazlı açısal kırpma**

Aday yer değiştirme vektörü ve açısı:

```
d_ham = P_ham − P_{n-1}
θ_ham = atan2(d_ham.y, d_ham.x)
```

**Kaçak (RUNNER)** — açı değişimi maksimum dönüş hızıyla kırpılır, dönüş
sertleştikçe izin verilen hız düşer:

```
Δθ        = wrapPi(θ_ham − θ_{n-1})
Δθ_geçerli = clamp(Δθ, −ω_max·Δt, +ω_max·Δt)
θ_n       = wrapPi(θ_{n-1} + Δθ_geçerli)
V_izin    = V_kaçak · (1 − α · |Δθ_geçerli| / π)
```

**Avcı (HUNTER)** — `ω_max = ∞` kabul edilir, açısal kırpma uygulanmaz
(`θ_n = θ_ham`, `V_izin = V_avcı`). Ancak durum tutarlılığı için `θ_{n-1}`
yine de güncellenir; rol değişiminde gerekir.

**3 — Mesafe (slack bütçesi) ve daralan arena kırpması**

```
d_max     = V_izin · Δt + slack
d_geçerli = min(‖d_ham‖, d_max)
slack     = clamp(slack + V_izin · Δt − d_geçerli, 0, S_max)     S_max = 25 birim
P_n       = P_{n-1} + d_geçerli · (cos θ_n, sin θ_n)
```

**Slack bütçesi.** Paketler ağda kümelenince ölçülen `Δt` küçülür ve
istemcinin meşru hareketi kırpılır. Tolerans paket başına sabit bir ek
değil, **zamanla biriken ve tavanlı bir bütçedir**: oyuncu izin verilenden
az hareket ettiği her adımda aradaki fark slack'e eklenir (en fazla
`S_max`), izin verilenden fazla hareket ettiğinde slack'ten düşülür. Bu,
hız sınırlayıcıdaki token bucket'ın mesafe cinsinden aynısıdır.

Anahtar özellik: sınır paket başına değil, **toplam mesafe üzerindedir.**
Herhangi bir `T` saniyelik pencerede kat edilebilecek en fazla mesafe
`V · T + S_max` olur ve paket hızından bağımsızdır.

Düzeltmeden sonra aynı ölçüm (dolu slack ile, hareketi kurulmuş istemci;
10, 25, 60 ve 120 Hz'de **aynı**):

| Rol | İlk 1 sn | Sonraki 1 sn | Nominal |
|---|---|---|---|
| Avcı | 250,0 | 225,0 | 225 |
| Kaçak | 285,0 | 260,0 | 260 |

Kaçak/avcı oranı sürekli harekette nominal orandır (260 / 225, +%15,6).

- Yarı başlangıcında (spawn) slack doludur (`S_max`).
- `DISCONNECT_GRACE` dönüşünde slack **yeniden doldurulmaz**; kopma anındaki
  değeriyle devam eder. Slack yalnızca zamanla dolar; kopup dönmek bedava
  bütçe vermez.
- Duran oyuncunun slack'i `S_max`'e kadar dolar, orada kalır.

**Neden paket başına sabit değil.** İlk tasarım `d_max = V_izin · Δt +
B_jitter` idi; `B_jitter` (25 birim) her pakete ekleniyordu. Doğrulama hattı
üzerinde ölçüldüğünde (hep uzak bir noktayı isteyen istemci, 1 saniyede kat
edilen mesafe):

| Rol, paket hızı | 1 sn'de | Nominal |
|---|---|---|
| Avcı, 25 Hz | 850,0 | 225 |
| Kaçak, 25 Hz | 885,0 | 260 |
| Avcı, 60 Hz | 1774,5 | 225 |
| Kaçak, 60 Hz | 1817,2 | 260 |

Üç kusur: efektif hız nominalin 3–8 katıydı; sınır paket hızının
fonksiyonuydu; ve iki role aynı sabit eklendiği için kaçak ile avcı
arasındaki hız farkı %15'ten %4'e iniyor, kaçağın avcıdan belirgin hızlı
olmasına dayanan denge ortadan kalkıyordu. 885 birim/sn ile 1000 birimlik
arena çapı 1,1 saniyede geçiliyordu; nominal hızda ~4 saniye sürer.

Sunucu, aktif yarı süresine göre güncel yarıçapı kendisi hesaplar: son 15
saniyede `R_0 → R_min` lineer daralma. `‖P_n‖ > R(t_yarı)` ise konum sınır
çemberi üzerine ölçeklenir.

#### Kısılmalı `clamp` bildirimi

Kırpma sunucu durumunda **her ihlalde anında uygulanır.** Ancak istemciye
geri gönderilen `clamp` uyarısı oyuncu başına en fazla 2–3 mesaj/sn (min
400 ms aralık) ile kısıtlanır. Böylece hile deneyen istemci sınır değerlerini
deneme yanılmayla hızlıca tarayamaz ve sunucu geri bildirim taşmasına
zorlanamaz.

### 1.3 Halka Tamponu ve Simetrik Yakalama Kontrolü

Sunucu her oyuncu için son 2,5 saniyelik doğrulanmış `(ServerTs, X, Y)`
kayıtlarını sabit boyutlu bir halka tamponda tutar (25 Hz'de ~64 kayıt).
Aynı tampon hem yakalama hizalaması hem botun 2000 ms gecikmeli algısı için
kullanılır.

**Simetrik kontrol kuralı.** HUNTER veya RUNNER fark etmeksizin, hangi
oyuncudan geçerli bir konum paketi gelirse gelsin (`t = ServerTs`), gelen
paketin doğrulanmış konumu **diğer oyuncunun** halka tamponundaki geçmişe
dönük nedensel pencereyle kıyaslanır:

```
t_diğer ∈ [t − 150 ms, t]
```

Bu aralıktaki ardışık örnekler arasında lineer enterpolasyonla en kısa mesafe
`d_min` hesaplanır. `d_min ≤ D_capture` ise yakalama kesinleşir ve yarı
sonlandırılır.

Simetri, yakalama kararının ağda kimin paketinin önce ulaştığına bağlı
olmasını ortadan kaldırır.

> **Uygulama notu:** Pencereye *giren* ilk segment (önceki örnek pencere
> dışında, sonraki içinde) atlanmamalıdır; aksi halde yakalama birkaç
> milisaniye geç tespit edilir.

İleri pencere (`t + 50 ms`) **kullanılmaz**: paket geldiğinde diğer oyuncunun
tamponunda gelecek kayıt bulunmaz.

### 1.4 Sabit Aralıklı Beraberlik Bozucu

İki yarı sonunda yakalama sayıları ve süreleri eşitse, kazananı belirlemek
için **ortalama avcı baskı mesafesi** kullanılır.

Paket hızı farklılıklarının metriği bozmaması için örnekleme gelen paketlerde
değil, **sabit 200 ms'lik sunucu zamanlayıcısıyla** (5 Hz) yapılır. Yalnızca
`HALF_1_ACTIVE` ve `HALF_2_ACTIVE` durumlarında çalışır.

**Mesafe yalnızca o yarıda `HUNTER` rolünde olan oyuncunun baskı toplamına
eklenir.** İki oyuncuya birden eklenirse iki yarının toplamı daima eşit çıkar
ve yakalamasız her maç berabere biter.

İki yarı sonunda ortalama baskı mesafesi daha düşük olan (avcıyken rakibine
daha çok baskı kuran) oyuncu kazanır.

**DRAW kuralı.** Yakalama sayıları, süreleri ve baskı ortalamaları eşit
kalırsa (`|d̄₁ − d̄₂| ≤ 10⁻³`) maç berabere biter. Puanlı maçta her iki oyuncu
+20 puan alır; aktif galibiyet serileri bozulmaz ama artırılmaz.

### 1.5 Bilgi Eşitlikli ve Doğrulamadan Geçen Bot

**Ortak doğrulama hattı.** Botun ürettiği hedef koordinatlar doğrudan oda
durumuna yazılmaz; tıpkı insan istemciler gibi aynı hız, açısal atalet ve
`R(t)` doğrulama hattından geçer. Botun da kendi 2,5 saniyelik halka tamponu
bulunur.

Bu kural iki işe yarar: bot fiziksel kuralları aşamaz, ve her bot maçı
doğrulama hattının canlı testine dönüşür.

**Bot Avcı** (oyuncu kaçak): kaçağın son doğrulanmış konumu ve türetilmiş yön
vektörü üzerinden kestirimci ön-kesme uygular; kaçağı `R(t)` sınırına
sıkıştıracak açıdan yaklaşır. Oyuncu botu doğrudan görmediği için mekanik
hareketi fark edilmez.

**Bot Kaçak** (oyuncu avcı): insan kaçakla **tam bilgi eşitliğine** sahiptir.
Tek girdisi, halka tamponundaki `ServerTs − 2000 ms` anına ait avcı
konumudur. Üzerine 180–260 ms insansı tepki gecikmesi ve arena sınırından
kaçınma sezgisi eklenir. Bot avcının gerçek anlık konumunu bilmediği için
tahmin yürütmek zorunda kalır; yanlış yöne kaçma ve son an manevraları doğal
olarak ortaya çıkar.

> **Uygulama notu:** Botun efektif güncelleme hızı insan oyuncununkiyle
> eşitlenmelidir; aksi halde bot sistematik olarak avantajlı veya dezavantajlı
> olur.

**Bot kaçağın duvar kuralı.** Bot, doğrulama hattının düzelteceği bir hedef
istemez; hat yine de her adımı denetler. Bunun için:

- **Dönüş yarıçapı kadar önce döner.** `turnRadius = V_kaçak / ω_max +
  tampon` (260 / 4,2 + 15 ≈ 76,9 birim; tampon arenanın daralması içindir).
  Bot `R(t) − turnRadius` çizgisini geçtiğinde ve dışa yönelmişse ya da
  dışa kaçmak istiyorsa duvar boyunca, hafif içe dönük (~27°) bir yöne döner.
  Duvar boyunca hangi yöne: zaten gittiği yöne, böylece duvarda hiç tersine
  dönmez (tersine dönüş kaçağı çemberin dışına savurur). Duvara tam karşıdan
  geliyorsa, son gördüğü (`ServerTs − 2000 ms`) avcıdan uzaklaşan yöne.
- **Bir dönüş yarıçapı içeride kademeli.** Bu çizgiden bir `turnRadius`
  daha içeride, kaçış yönünün dışa bileşeni kademeli azaltılır; bot çizgiye
  zaten duvara paralel koşarak varır, çizginin iki yanında zikzak çizmez.
- **Adımını hattın göreceği `Δt` ile planlar.** Sabit tick süresi değil,
  son doğrulanmış adımından bu yana geçen gerçek süre. Erken veya geç gelen
  bir tick botun sınırlarını hattınkiyle aynı biçimde değiştirir.
- **Son çare: adımı kısaltır.** Hedef yine de arena dışına düşecekse adımı
  çembere kadar kısaltır; daha az hareket etmek her zaman meşrudur.

Kabul ölçütü: iki tam yarılık bot maçında `bot_clamp == 0`; düzenli ve
düzensiz tick'lerle, duran, kovalayan ve duvar boyunca kaçan insanla.

---

## 2. Bağlantı Kopması ve Yeniden Bağlanma

Maç aktifken bir oyuncunun WebSocket bağlantısı koparsa oda
`DISCONNECT_GRACE` durumuna geçer ve 5,0 saniyelik geri sayım başlar.

| Konu | Davranış |
|---|---|
| **Yarı sayacı** | Duraklatılır. `rem_ms` ve `R(t)` daralması dondurulur. Avcı koparsa av süresinden kaybetmez; kaçak son saniyelerde bilerek kopup süreyi eritemez |
| **Yakalama kontrolü** | Duraklatılır. Kopan oyuncu `P_anchor`'da sabit kaldığı için geçici ağ kopması otomatik yakalamaya dönüşmez |
| **200 ms baskı örneklemesi** | Duraklatılır. Baskı mesafesi çarpıtılmaz |
| **Dönüş (RESUME)** | 5,0 sn içinde yeniden bağlanırsa sunucu `snap` gönderir, bölüm 1.2'deki dönüş sıfırlaması uygulanır, yarı kaldığı saniyeden devam eder |
| **Süre dolumu (FORFEIT)** | Kopan oyuncu hükmen yenik sayılır, maç `FINISHED`'a geçer, kalan oyuncu anında yeni eşleşmeye dönebilir |

**Sayaç oyuncu başınadır.** Kopma oda başına değil, oyuncu başına bir
olaydır; her kopan oyuncunun kendi 5,0 saniyelik son tarihi vardır.

| Kural | Davranış |
|---|---|
| Son tarihin başlangıcı | Oda o oyuncuyu beklemeye başladığı an: aktif yarıda veya `DISCONNECT_GRACE` içinde koptuysa kopma anı; geri sayım / rol değişimi sonunda kopuk bulunduysa odanın `DISCONNECT_GRACE`'e girdiği an |
| Bağımsızlık | Bir oyuncunun sayacı diğerininkini etkilemez; ikinci kopan oyuncu kendi tam 5,0 saniyesini alır |
| Oda durumu | Herhangi bir oyuncu kopukken oda `DISCONNECT_GRACE`'te kalır |
| Dönüş | Son tarihinden önce dönen oyuncunun sayacı silinir. Oda, **tüm** oyuncular bağlı olduğunda devam eder |
| Süre dolumu | Oyuncu kendi son tarihi dolunca hükmen yenilir; sonradan dönmesi bunu geri almaz. Rakibi o an bağlıysa maç hemen biter. Rakibi de kopuksa (ve kendi süresi dolmamışsa) oda rakibi beklemeye devam eder: rakip zamanında dönerse hükmen galip gelir |
| İkisinin de süresi dolarsa | Maç geçersiz, puansız, kazanan yok |
| Maç toplamı | Her oyuncunun maç boyunca biriken bekleme süresi tutulur. Tek kopmada en fazla `T_grace` (5 sn), maç toplamında en fazla `T_grace_total` (15 sn): bir kopmanın son tarihi `min(T_grace, T_grace_total − kullanılan)` kadar sonradır. Toplamı tükenmiş oyuncu koptuğu anda hükmen yenilir |

Biriken süre, odanın o oyuncuyu beklediği süredir (son tarihin başladığı
andan dönüşe kadar). Toplam olmadan, dönüp tekrar kopan oyuncu her seferinde
yeni 5 saniye alır ve maçı istediği kadar kilitleyebilirdi.

Oda başına tek sayaç şu hatayı üretiyordu: A `t0`'da, B `t0 + 4,9 sn`'de
kopar, A `t0 + 4,95 sn`'de döner; sayaç `t0 + 5 sn`'de dolar ve B yalnızca
0,1 saniyelik yokluktan sonra hükmen yenilir.

**`COUNTDOWN_1` / `ROLE_SWAP` sırasında kopma.** Geri sayım bittiğinde
oyunculardan birinin bağlantısı kopuksa oda aktif yarıyı **başlatmadan**
doğrudan `DISCONNECT_GRACE` durumuna geçer. Aksi halde yarı, hareket
edemeyen bir oyuncuyla başlar ve grace sayacı hiç tetiklenmez.

**İki oyuncu da koparsa.** İki oyuncunun da kendi son tarihi dolarsa maç
**geçersizdir**: kazanan yoktur, puan yazılmaz, kimseye hükmen
yenilgi işlenmez. `both_disconnected` sayacı artırılır (bölüm 5); bu durum
beklenenden sık görülürse sorun oyuncuda değil bağlantı tarafındadır.

**Sunucu kapanışı.** Sunucu kapanırken (rollout dahil) odalar iptal edilir:
hükmen yenilgi **yazılmaz**, skor katmanına dokunulmaz. Aksi halde her
rollout iki oyuncuyu da "terk etmiş" sayar. Maç ortasındaki oyuncular maçı
kaybeder; bkz. bölüm 11.

**Oda paniği.** Odanın goroutine'i paniklerse panik yakalanır ve loglanır,
maç **geçersiz ve puansız** biter (kazanan yok, skor katmanı çağrılmaz),
süreç ve diğer odalar ayakta kalır. Panikten sonra maçı puanlı bitirmek,
durumu bozuk bir odadan skor yazmak olurdu. `room_panic` sayacı artırılır
(bölüm 5).

**Bağlı sayılma.** Bir oyuncu, odaya katılmış en az bir bağlantısı varken
bağlıdır (ikinci sekme tek bağlantının kopmasını örter).

---

## 3. İstemci Mimarisi, Ekran Yerleşimi ve Kontrol

### 3.1 Kendi karakteri — local prediction

Oyuncu yön girdisini uyguladığı anda istemci karakteri yerel hız formülüyle
**anında** hareket ettirir ve 25 Hz'de (40 ms) tek birleşik paket gönderir.
Sunucu reconciliation yapmadığı için istemci kendi hareketini asla geri
zıplatmaz; yalnızca sunucudan `clamp` geldiğinde yumuşak düzeltme uygular.

### 3.2 Rakip karakter — remote interpolation

Rakip paketleri istemcide **60 ms'lik render tamponuna** alınır ve son iki
paket arasında lineer enterpolasyonla çizilir. Paket gecikirse son hız
vektörü (`vx`, `vy`) üzerinden en fazla 80 ms dead-reckoning yapılarak donma
gizlenir.

### 3.3 2000 ms ışık izi — nerede uygulanır

**Gecikme sunucuda değil istemcidedir.** Sunucu doğrulanmış `pos` paketlerini
odaya gecikmesiz yayınlar; `RUNNER` rolündeki istemci avcının konumlarını
2,5 saniyelik yerel tamponunda tutar ve ekrana yalnızca **2000 ms öncesine**
ait noktayı, arkasında sönümlenen kuyrukla çizer.

İstemci bu gecikmeyi **kendi yerel alış zamanlarına göre** uygular; sunucu
saatiyle hizalama gerekmez. Paketlerdeki `sts` alanı sıralama ve titreşim
düzeltme için kullanılır, çizim gecikmesinin referansı değildir. Bu, saat
kayması sorununu tamamen ortadan kaldırır.

`HUNTER` rolündeki istemci bu tamponu kullanmaz; kaçağı canlı çizer.

### 3.4 Ekran yerleşimi ve kontrol

**Tek tuval.** Arena tam daire olduğu için ekranın merkezine oturan 1:1
oranlı bir `<canvas>` kullanılır. Üst şeritte `[Rol Rozeti] [00:34] [● ○]`,
alt veya yan panelde canlı skor tablosu özeti.

**Masaüstü:** Fare imleci yön ve hız oranını (%0–100) belirler; tıklama
gerekmez. Alternatif olarak WASD / yön tuşları desteklenir.

**Mobil:** Ekranın herhangi bir yerine parmak basıldığı nokta merkez kabul
edilen **yüzer sanal analog** (floating thumbstick). Sabit ekran butonu
yoktur; tek elle dikey tutuşta başparmakla oynanır.

---

## 4. Skor Tablosu ve Suistimal Korumaları

**Redis anahtarları**

- `lb:ghosttag:alltime` — genel sıralama
- `lb:ghosttag:daily:YYYY-MM-DD` — günlük sıralama (UTC gün anahtarı, geri
  dönüş kancası)
- `player:stats:{player_id}` (HASH) — takma ad, toplam galibiyet, aktif seri,
  en hızlı yakalama

**Puan değişimi (yalnızca insan-insan maçlarda)**

| Sonuç | Δ Puan |
|---|---|
| Galibiyet (yakalama ile) | `40 + HızBonusu + SeriBonusu` |
| Galibiyet (beraberlik bozucu ile) | `40 + SeriBonusu` (HızBonusu = 0) |
| Mağlubiyet (tamamlanan maç) | +5 (katılım) |
| Berabere | Her iki oyuncuya +20, seriler korunur (artırılmaz) |
| Hükmen terk | Terk eden −25 ve serisi **sıfırlanır**; kalan +40 (HızBonusu = 0, SeriBonusu = 0), serisi korunur ama **artırılmaz** |

**Seri bonusu:**

```
SeriBonusu = min(win_streak × 5, 25)
```

`win_streak`, **bu galibiyet dahil edilmiş** değerdir: ekranda gösterilen seri
ile ödüllendirilen seri aynıdır. İlk galibiyet +5, tavan 5 maçlık seride +25.
Mağlubiyet seriyi sıfırlar; beraberlik ve hükmen kazanılan maç seriyi ne
bozar ne artırır.

**Hız bonusunun kaynağı:** kazananın **kendi avcı yarısındaki** yakalama
süresi (`dur_ms`). Kazanan beraberlik bozucuyla belirlendiyse 0'dır.

**Hız bonusu — birim milisaniye:**

```
HızBonusu = floor( max(0, T_yarı_ms − dur_ms) / 1000 )
T_yarı_ms = 45000 ms,  maksimum +45 puan
```

**Suistimal korumaları**

- **Bot maçları hariç** (`is_bot_match == true`): puan üretmez, Redis'e
  yazılmaz.
- **Aynı çift koruması:** aynı iki `player_id` arasında aynı UTC günü içinde
  oynanan tekrar maçlar puan üretmez. İki hesabın anlaşarak sırayla terk edip
  net puan kasmasını engeller.

**Skor yazım yolu.** WebSocket servisi maç sonucunu doğrudan Redis'e yazar;
anahtarları ve işlemi `internal/leaderboard` sahiplenir, oyun paketi yalnızca
bir arayüz görür. Gerekçe: `match_end.deltas` seri bonusunu içerir, yani maç
bitiminde seri durumu **senkron** okunmak zorundadır.

- Tek atomik işlem: aynı-çift-gün anahtarı, **oda kimliğiyle idempotans
  anahtarı**, seri okuma, iki sıralama ve `player:stats` yazımı. Aynı odanın
  yeniden denenmesi çift puan yazmaz, ilk sonucun aynısını döndürür.
- Yazma başarısız olursa `match_end` `ranked: false` ve sıfır `deltas` ile
  gider; hata sayılır ve sonuç loglanır. Oyuncuya yazılmamış puan gösterilmez.
- Başarılı yazımdan sonra, puanı değişen her oyuncu için mevcut yayıncıyla
  `LeaderboardUpdated` yayınlanır; aksi halde diğer istemcilerin skor tablosu
  bayat kalır. Bu olayın artık iki üreticisi vardır, bkz. bölüm 11.

**Hesap yükseltme uyumu.** Skorlar `player_id` (JWT `sub`) anahtarına
yazıldığı için misafir oturumu ileride kalıcı hesaba bağlandığında puan ve
seri kayıpsız devreder.

---

## 5. Telemetri ve Başarı Metrikleri

| Metrik | Tanım | Hedef |
|---|---|---|
| Tur tamamlama oranı | Başlayan maçların `FINISHED`'a ulaşma yüzdesi | > %85 |
| İkinci tur oranı | Maçı bitirenlerin 30 sn içinde ikinci maça başlama oranı | — |
| D1 dönüş | Aynı `player_id` ile ertesi gün tekrar oynama oranı | — |
| `ranked_match_ratio` | Puanlı maç / toplam maç | — |
| Bot/insan maç oranı | — | — |
| `clamp` tetiklenme sıklığı | Doğrulama hattının kırpma sayısı | — |
| Maç sonu RTT dağılımı | — | — |
| `join_timeout` | `T_join` içinde odaya katılmayan oyuncu yüzünden iptal edilen maç sayısı | — |
| `both_disconnected` | İki oyuncunun da kopmasıyla geçersiz sayılan maç sayısı | — |
| `room_panic` | Paniklediği için geçersiz bitirilen oda sayısı | 0 |
| İlk yarı avcısının kazanma oranı | İki yarısı da oynanarak sonuçlanan insan-insan maçlarda `first_hunter_won / (first_hunter_won + second_hunter_won)` | ~%50 |

**İlk yarı avcısı oranı neden ölçülür:** rol sırası teoride küçük bir
asimetri taşır; ikinci yarının avcısı geçmesi gereken süreyi bilir. Oran
%50'den belirgin saparsa rol sırası avantaj yaratıyor demektir.

Sayaçlar `gamemesh_ws_game_events_total{event="…"}` serisinde tutulur; oda
veya oyuncu etiketi taşımaz.

**İkinci tur oranı belirleyici metriktir:** "biri bu oyunu ikinci kez oynar
mı?" sorusunun doğrudan ölçümü ve projenin gerçek oyuna dönüşüp
dönüşmeyeceği kararının dayanağı.

---

## 6. Mesaj Sözleşmesi

GameMesh sınırları: oyuncu başına **60 mesaj/sn** (patlama 120), mesaj başına
en fazla **1024 bayt**.

| Tip | Yön | Sıklık | Örnek yük | Boyut |
|---|---|---|---|---|
| `pos` (gelen) | Client → Room | 25 Hz (40 ms) | `{"t":"pos","seq":142,"cts":173168,"x":120.4,"y":-45.2,"vx":180,"vy":-90}` | ~76 B |
| `pos` (yayın) | Server → Room | 25 Hz (40 ms) | `{"t":"pos","from":"p_9f2a","sts":173168040,"x":120.4,"y":-45.2,"vx":180,"vy":-90}` | ~84 B |
| `ping` / `pong` | Client ↔ Server | 0,5 Hz | `{"t":"ping","cts":173168}` | ~28 B |
| `snap` | Server → Room | Olay bazlı | `{"t":"snap","state":"HALF_1_ACTIVE","sts":173168000,"rem_ms":45000,"r":500,"players":[{"id":"p_9f2a","role":"HUNTER","x":-225,"y":0,"theta":0,"connected":true}, …]}` | ~260 B |
| `clamp` | Server → tek istemci | İhlalde, max 2,5 Hz | `{"t":"clamp","x":118.0,"y":-44.0,"theta":1.52}` | ~52 B |
| `half_end` | Server → Room | 2 kez/maç | `{"t":"half_end","half":1,"reason":"CAPTURE","dur_ms":28400,"hunter":"p_9f2a","runner":"p_1b3c"}` | ~145 B |
| `match_end` | Server → Room | 1 kez/maç | `{"t":"match_end","winner":"p_9f2a","is_draw":false,"ranked":true,"deltas":{"p_9f2a":61,"p_1b3c":5}}` | ~165 B |

**Zarf.** Tablodaki yükler GameMesh zarfının `data` alanında taşınır; boyut
sütunu yalnızca yükü sayar, zarf ~40 bayt ekler (en büyük paket sınırın
%25'inde olduğu için sorun değil).

- Gelen: `{"action":"relay","room":"<id>","data":<yük>}`. Mevcut relay boyut,
  hız ve üyelik kontrolleri önce çalışır. Validated odada `data` açılır ve
  odanın olay döngüsüne verilir; opaque yayın **yapılmaz**, tanınmayan `t`
  reddedilir.
- Giden: `{"type":"Game","room":"<id>","data":<yük>}`. `Relay` tipi "eşten
  gelen, yorumlanmamış bayt" demektir; `Game` tipi sunucu kararıdır. Ayrım
  kablo üzerinde görünür kalır.

**`sts` birimi:** oda başlangıcından itibaren milisaniye (monoton). İstemci
çizim gecikmesini yerel alış zamanından uyguladığı için (bölüm 3.3) `sts`
yalnızca sıralama ve titreşim düzeltme içindir; duvar saati taşımaz.

**`match_end` örneği:** `40 + floor((45000 − 28400) / 1000) + 5 = 61`
(yakalama 28,4 sn'de, seri = 1).

**Yayın yönündeki `pos` paketinde** `seq` ve `cts` temizlenir; `from` alanı
sunucu tarafından JWT kimliğinden yazılır, `sts` sunucu alım zamanıdır.
İstemci iddiası hiçbir alanda taşınmaz — `vx`/`vy` yalnızca dead-reckoning
ipucudur.

**`match_end` beraberlikte:** `winner` boş dize, `is_draw: true`.
Puansız maçta (`ranked: false`) `deltas` tüm oyuncular için 0'dır.

**Sınır sağlaması:** istemci çıkışı 25 + 0,5 = **25,5 msg/sn** (sınırın
%42,5'i); sunucu yayını en fazla 27,5 msg/sn; en büyük paket ~260 bayt
(sınırın %25'i).

---

## 7. Oda Durum Makinesi

```
[Matchmaking Tick (5s): 2 İnsan veya 1 İnsan + Bot]
                       │
                       ▼
              ┌─────────────────┐   [T_join (10 sn) doldu]
              │     WAITING     │ ─────────────────────────► İPTAL
              │ • İnsan oyuncular│   (puansız, hükmen yenilgi yok,
              │   odaya katılır │    join_timeout sayacı)
              └────────┬────────┘
                       │ [tüm insan oyuncular katıldı]
                       ▼
              ┌─────────────────┐
              │   COUNTDOWN_1   │ (2.0 sn — spawn noktaları + roller)
              └────────┬────────┘
                       ▼
┌──────────────────────────────────────────────────────────┐
│                      HALF_1_ACTIVE                       │
│ • Max süre: T_half_ms (45000 ms)                         │
│ • Son 15000 ms: R(t) daralır (R_0 → R_min)               │
│ • Her 200 ms: sabit aralıklı baskı mesafesi örneklenir   │
└─────────┬────────────────────────────────────┬───────────┘
          │                                    │
 [Bağlantı koptu]                 [Simetrik yakalama VEYA süre doldu]
          │                                    │
          ▼                                    ▼
┌────────────────────┐              ┌────────────────────┐
│ DISCONNECT_GRACE   │              │     ROLE_SWAP      │ (3.0 sn)
│ • Max 5.0 sn       │              │ • Durumlar sıfırla │
│ • Sayaç DURAKLAR   │              │ • Spawn'a yerleştir│
│ • Yakalama DURAKLAR│              │ • Tamponları sil   │
│ • Örnekleme DURAKLAR│             └─────────┬──────────┘
└───┬────────────┬───┘                        ▼
    │            │                  ┌────────────────────┐
[Döndü]      [5s doldu]             │   HALF_2_ACTIVE    │ (aynı kurallar)
    │            │                  └─────────┬──────────┘
    ▼            │                            │
(Sıfırla &       │                            ▼
 devam et)       │                  ┌────────────────────┐
                 └─────────────────►│      FINISHED      │
                                    │ • Kazanan belirle  │
                                    │ • Ranked kontrolü  │
                                    │ • match_end yayını │
                                    └────────────────────┘
```

**İlk yarıda kim avcı.**

- **İnsan-insan:** rastgele, ancak tohum **oda kimliğinden** türetilir. Aynı
  oda her zaman aynı rol sırasını verir; maç tekrar oynatılabilir ve rol
  dağıtımı log'dan doğrulanabilir. Eşleştirme oyuncuları rank sırasıyla
  yazdığı için "ilk oyuncu avcı" kuralı sistematik yanlılık üretirdi.
- **Bot maçı:** rastgele değil. **İnsan ilk yarıda KAÇAK'tır** (bot avcı).
  Kaçak rolünde oyuncu avcıyı hiç görmez, yalnızca izini görür; yeni
  ziyaretçinin ilk 45 saniyesi botun en doğal göründüğü yarı olur.

**`WAITING`.** Oda, ilk oyuncunun katılmasıyla açılır ve tüm insan oyuncular
katılana kadar bekler. Bu durumda geri sayım başlamaz, `R(t)` işlemez, konum
paketleri yoksayılır. `T_join` dolarsa maç iptal edilir: oyun hiç başlamadığı
için kimseye hükmen yenilgi yazılmaz, puan üretilmez; bağlı oyuncuya
`ranked: false`, sıfır `deltas` ve boş `winner` ile `match_end` gönderilir.

**`DISCONNECT_GRACE` sonu.** Tek oyuncu kopuksa hükmen yenilir; ikisi de
kopuksa maç geçersizdir (bölüm 2).

### Gelen konum işleme hattı (insan ve bot için ortak)

1. Oda `HALF_1_ACTIVE` veya `HALF_2_ACTIVE` değilse yoksay.
2. İnsan istemcisi için `seq <= LastSeq` kontrolü (bot iç tick kullanır).
3. `now := time.Now()` ile bölüm 1.2 doğrulaması: `has_prev_motion`, `ω_max`
   (kaçak için), `V_izin`, `R(t)`. Kırpma olduysa ve son bildirimden ≥ 400 ms
   geçtiyse ilgili istemciye `clamp` yolla.
4. Doğrulanmış `(now, validX, validY)` kaydını halka tampona yaz.
5. Doğrulanmış konumu ve `vx`, `vy` öngörü vektörünü odaya yayınla.
6. **Simetrik yakalama kararı:** gelen oyuncunun doğrulanmış konumunu diğer
   oyuncunun `[now − 150 ms, now]` penceresiyle karşılaştır;
   `d_min ≤ D_capture` ise yarıyı sonlandır.

> **Uygulama notu — eşzamanlılık:** `Tick` ve gelen paket işleme aynı oda
> durumunu değiştirir. "Tek goroutine, mutex yok" yaklaşımı ancak oda tek bir
> olay döngüsüyle serileştirilirse geçerlidir. Bu, entegrasyonun en büyük
> riskidir ve `go test -race` ile kanıtlanmalıdır.

**Karar — oda başına tek olay döngüsü.** Oda durumuna yalnızca odanın kendi
goroutine'i dokunur. Konum paketleri kayıplı bir kutuya (dolarsa düşer ve
sayılır), bağlantı değişikliği bildirimleri kayıpsız ayrı bir kanala girer;
ikisi de tek 40 ms'lik ticker ile aynı `select` içinde okunur. Geri sayım,
yarı sonu, grace süresi, 200 ms baskı örneklemesi ve bot adımı bu ticker'dan
türetilir; ayrı goroutine'de koşan zamanlayıcı geri çağrısı kullanılmaz. Oda
kodu saati parametre olarak alır, kendisi okumaz. `ServerTs`, paketin
soketten okunduğu andır ve oda içinde monotonluğu döngü zorlar. Hub, kendi
kilidini tutarken odaya bloklanan bir çağrı yapmaz.

**Bağlantı durumu seviye tetiklemelidir.** Hub odaya "şu oyuncu koptu" demez;
"şu oyuncunun durumu değişmiş olabilir" der ve oda gerçeği hub'a sorar. Kenar
tetiklemeli olaylar sıra bozulmasına açıktır: bir sekme kapanırken diğeri
katılırsa "koptu" bildirimi "bağlandı"dan sonra ulaşabilir ve oda bağlı bir
oyuncuyu kopmuş sanar. Olayı işlemek yerine güncel durumu okumak bu hata
sınıfını ortadan kaldırır.

---

## 8. Parametreler

Tüm sabitler `Config` içinde toplanır; koda gömülmez. Oynanış testinde
ayarlanacaktır.

| Parametre | Başlangıç değeri | Açıklama |
|---|---|---|
| `V_kaçak` | 260 birim/sn | Kaçak maksimum hızı |
| `V_avcı` | 225 birim/sn | Avcı maksimum hızı |
| `ω_max` | 4,2 rad/sn | Kaçak maksimum açısal hızı (avcıda ∞) |
| `α` | 0,45 | Dönüş hız cezası katsayısı — **kesinlikle 0 < α < 1** |
| `R_0` | 500 birim | Başlangıç arena yarıçapı |
| `R_min` | 280 birim | Daralmış arena yarıçapı |
| `D_capture` | 36 birim | Yakalama yarıçapı (~2,5 × görsel yarıçap) |
| `S_max` | 25 birim | Slack bütçesinin tavanı: ağ titreşim toleransı. Paket başına eklenmez, zamanla dolar (bölüm 1.2) |
| `Δt_max` | 0,25 sn | Doğrulama zaman adımının tavanı (taban yoktur) |
| `ε_hareket` | 10⁻³ birim | Bundan kısa yer değiştirme "duruyor" sayılır; `θ` güncellenmez |
| `Δt_initial` | 0,04 sn | Spawn / reconnect sonrası ilk paketin `dt` değeri |
| İstemci render tamponu | 60 ms | Rakip enterpolasyon gecikmesi |
| Dead-reckoning tavanı | 80 ms | Paket gecikmesinde öngörü süresi |
| `T_half` | 45 sn | Yarı süresi |
| `T_shrink` | 15 sn | Daralma penceresi (yarı sonu) |
| `T_countdown` | 2 sn | Başlangıç geri sayımı |
| `T_roleswap` | 3 sn | Rol değişim arası |
| `T_grace` | 5 sn | Tek kopmada bekleme süresi, **oyuncu başına** (bölüm 2) |
| `T_grace_total` | 15 sn | Bir oyuncunun maç boyunca toplam bekleme süresi |
| `T_join` | 10 sn | `WAITING` durumunda oyuncuların odaya katılması için süre |
| Yakalama penceresi | 150 ms | Simetrik kontrol geçmiş penceresi |
| Bot görüş gecikmesi | 2000 ms | Bot kaçağın avcıyı gördüğü gecikme |
| Bot duvar tamponu | 15 birim | Bot kaçağın duvara dönmeye başladığı mesafe: `V_kaçak / ω_max` + bu tampon (bölüm 1.5) |
| Baskı örnekleme aralığı | 200 ms | Beraberlik bozucu tick'i |
| `clamp` bildirim aralığı | 400 ms | İstemciye kırpma uyarısı kısıtı |

`α ≤ 0` veya `α ≥ 1` değerleri geçersizdir: `α = 1` 180° dönüşte hızı
sıfırlar, `α > 1` negatif hız üretir. Yapılandırma doğrulaması bunu
reddetmelidir.

---

## 9. Mimari Notlar

### Not A — Çift yollu iletim mimarisi

Ghost Tag ile birlikte GameMesh'te iki ayrı iletim modu belgelenmelidir:

| Mod | Davranış |
|---|---|
| **opaque-relay** | Genel amaçlı. Sunucu yük içeriğini yorumlamaz; yalnızca gönderen kimliğini (`from`) JWT'den yazarak odaya yayınlar |
| **validated-room** | Ghost Tag odaları. Sunucu konum paketlerini açar, süreklilik/atalet/sınır doğrulaması uygular ve ayrık oyun kararlarını (yakalama, yarı geçişi, maç sonucu) üretir |

Odanın modu istemciden değil, eşleştirmenin oda kaydından (`mode` alanı)
gelir; alanı olmayan oda opaque-relay'dir.

README ve `docs` içindeki mevcut "sunucu içeriği yorumlamaz, yalnızca taşır"
tanımı bu ayrımı içerecek şekilde güncellenmelidir.

### Not B — Benchmark kuralı

**Doğrulama hattının sunucu işlem süresi üzerine varsayım yazılmaz.** Sunucu
çekirdeği tamamlandığında `scripts/latencybench` doğrulama hattı devre dışı ve
devrede olacak şekilde çalıştırılır; ölçülen fark `docs/latency.md` ve
`README.md` dosyalarına işlenir.

Mevcut ölçülmüş değerler (referans): sunucu payı mesaj başına 0,02–0,13 ms;
prod'da uçtan uca p50 ~40 ms, tamamı ağ.

Ölçüm üç satırdır:

1. **Opaque relay, değişiklik öncesi ve sonrası** (`main` ile bu dal).
   Validated odalar eklenirken opaque relay'in çerçevesi hub'ın okuma kilidi
   altında kurulur oldu. Bu yol k6 senaryolarına ve yayınlanmış rakamlara
   hizmet eder; gerileme olursa README'deki sayılar yanlışa düşer.
2. **Validated oda**, aynı boyutta `pos` yükü ve aynı frekansla.
3. **`go test -bench`**, yalnızca paket başına yol (doğrulama + halka +
   yakalama); 1 ile 2 arasındaki farkın ne kadarının hesap, ne kadarının JSON
   ve döngü atlaması olduğunu ayırır.

**Validated modlu bench yalnızca yerelde koşar, prod'da asla.** Bench maçları
Ghost Tag sıralamasına puan yazar. İkinci emniyet olarak bench odaları
puansız bayrağıyla (`ranked: false`) oluşturulur; kural ihlal edilse bile
yazma olmaz.

---

## 10. v1 Kapsamı

| v1 kapsamında | v1 dışında |
|---|---|
| Tek mod: 1v1, iki yarılı, rol değişimli | İkinci mod, harita seçimi, zorluk seviyesi |
| Misafir JWT ile tek tıkla anında giriş | E-posta/şifre kaydı, profil düzenleme |
| 5 sn matchmaking + bulunamazsa doğrulamalı asimetrik bot | Geçmiş insan maçlarının Ghost replay arşivi |
| Sunucu yön/hız doğrulaması + `R(t)` arena daralması | Sunucu-istemci reconciliation, geçmiş geri sarma |
| Masaüstü (fare/klavye) + mobil (yüzer thumbstick) | Özel oda kurma, arkadaş davet linki |
| Günlük ve genel Redis skor tablosu | Kozmetik mağaza, başarımlar, sohbet |

---

## 11. Bilinen Sınırlar

- **Tek sunucu örneği.** Oyun durumu ilgili WebSocket odasının belleğinde
  yaşar. Eşleştirme servisi aynı odadaki oyuncuları aynı sunucu örneğine
  yönlendirmek zorundadır; replikalar arası yayın yoktur.
- **Puansız maç oranı.** Bot maçlarının hariç tutulması ve aynı çift günlük
  puan kilidi birlikte uygulandığında, düşük eşzamanlı oyuncu havuzunda
  maçların büyük bölümü puansız gerçekleşebilir ve skor tablosu erken dönemde
  seyrek görünebilir. `ranked_match_ratio` metriği bunu izler.
- **Rollout sırasında maç kaybı.** Sunucu kapanırken odalar iptal edilir;
  maç ortasındaki oyuncular maçı sonuçsuz kaybeder (hükmen yenilgi yazılmaz).
  Tek sunucu örneği kısıtının doğrudan sonucudur.
- **`LeaderboardUpdated` olayının iki üreticisi var.** Leaderboard servisi
  (REST skor güncellemeleri) ve WebSocket servisi (Ghost Tag maç sonucu).
  Anahtarlar ve yazım mantığı `internal/leaderboard` içinde ortak olsa da
  olayın sahipliği tek serviste değildir.
- **Ayrık oyun mesajları da en-fazla-bir-kez.** `snap`, `half_end` ve
  `match_end` oyun trafiğiyle aynı gönderim kuyruğunu kullanır; kuyruğu dolu
  yavaş bir istemci bunları kaçırabilir. `snap` yeniden bağlanmada tekrar
  gönderilir.
- **Kümelenmiş paketlerde dönüş kırpılır ve fark, istemci düzeltene kadar
  büyür.** Mesafenin slack bütçesi var, dönüşün yok (bilinçli seçim,
  bölüm 1.2). Paketleri ağda kümelenen ve o sırada en yüksek hızda dönen bir
  kaçağın kümedeki paketleri yaklaşık sıfır `Δt` görür ve dönüşü kırpılır.
  Kaybedilen açı, istemci en yüksek hızda dönmeye devam ettikçe geri
  kazanılmaz: sonraki zamanında gelen paketler de kırpılır. Ölçülen (25 Hz,
  80 ms boşluk ardından 1 ms arayla üç paket, sonra üç zamanında paket):
  sunucu yönü istemcinin 0,17 rad gerisinde kalır; konum farkı kümeden
  sonra 5,1 birim, üç zamanında paketten sonra 12,1 birimdir ve paket başına
  2–3 birim büyür. İstemci `clamp` mesajını uygulayınca kapanır; `clamp` en
  fazla 400 ms'de bir gönderildiği için bu, en kötü durumda ~10 paketlik bir
  sapmadır.
- **Dönüş hız cezası paket başına uygulanır.** `V_izin` tek adımın
  dönüşünden hesaplanır, dolayısıyla en yüksek hızda dönen bir kaçağın
  1 saniyede kat edebildiği mesafe paket hızına hafifçe bağlıdır. Ölçülen:
  10 Hz'de 244,36; 25 Hz'de 253,74; 60 Hz'de 257,39; 120 Hz'de 258,70 birim
  (`260 · (1 − α · ω_max / (π · hız))`). Sık gönderen istemci dönerken en
  fazla %6 daha hızlıdır; düz koşuda fark yoktur.
- **Spawn sonrası ilk saniye küçük bir pay taşır.** İlk paket gerçek bekleme
  süresi yerine sabit `Δt_initial` (0,04 sn) ile ölçüldüğü için, taze
  spawn'dan sonraki ilk saniyede kat edilebilen mesafe `V + S_max`'i en fazla
  `V · Δt_initial` kadar aşar ve paket hızına hafifçe bağlıdır: avcı 25 Hz'de
  250,0, 60 Hz'de 255,3; kaçak 285,0 ve 291,1. Hareket kurulduktan sonra her
  1 saniyelik pencere tam `V + S_max` ile sınırlıdır.
- **`pos` paketi JSON.** 76 baytın yarısı anahtar adları. v1 sınırların
  %7'sinde kaldığı için sorun değil; N oyunculu moda geçişte ikili format
  değerlendirilmelidir.

---

## 12. Çok Oyunculuya Genişleme Kapıları (1 Avcı + N Kaçak)

v1 iki kişilik çalışsa da aşağıdaki yapılar şimdiden çoğul tasarlanır:

- **Harita tabanlı oyuncu durumu:** `Player1 / Player2` yerine
  `Players map[PlayerID]*PlayerState`.
- **Rol ve yakalanma durumu:** her oyuncunun kendi `Role` ve
  `CapturedAt *time.Time` alanı. 1v1'de ilk yakalamada yarı biter; 1vN'de
  bitiş koşulu `active_runners == 0` olarak güncellenir.
- **Oda yayını:** tüm durum mesajları oyuncu dizisi taşır; istemci çizim
  döngüsü dizideki her elemanı rolüne göre çizer (canlı nokta veya 2 sn
  gecikmeli iz).
- **Sunucu örneği bağımsızlığı:** oyun kuralları tek sunucu varsayımına
  yaslanmaz; yalnızca eşleştirmenin aynı odaya aynı örneği atadığını varsayar.

---

## 13. Uygulama Kararları (2026-11-16)

Sunucu çekirdeği uygulanmadan önce verilen ve yukarıdaki bölümlere işlenen
kararlar:

| Konu | Karar | Bölüm |
|---|---|---|
| Eşzamanlılık | Oda başına tek olay döngüsü, tek 40 ms ticker, saat parametre | 7 |
| Zarf | Gelen `action: relay`, giden yeni `type: Game` | 6 |
| `sts` birimi | Oda başlangıcından itibaren ms | 6 |
| Oda modu | Eşleştirmenin oda kaydından, istemciden değil | 9 |
| Skor yazım yolu | Doğrudan Redis; oda kimliğiyle idempotans; sonra `LeaderboardUpdated` | 4 |
| `WAITING` durumu | `T_join` = 10 sn; dolarsa iptal, puansız, hükmen yenilgi yok | 7, 8 |
| İki oyuncu da koparsa | Maç geçersiz, puansız, kazanan yok | 2 |
| Sunucu kapanışı | Odalar iptal, hükmen yenilgi yazılmaz | 2, 11 |
| `win_streak` | Bu galibiyet dahil (sonraki) değer; tavan 5 maç | 4 |
| Hız bonusu | Kazananın kendi avcı yarısındaki yakalama süresi | 4 |
| `match_end` örneği | 58 hatalıydı (5'in katı seri bonusuyla üretilemez); 61 | 6 |
| Bench | Validated mod yalnızca yerelde; bench odaları puansız | 9 |
| Bağlantı durumu | Seviye tetiklemeli: oda bildirimi işlemez, gerçeği hub'a sorar | 7 |
| Oda paniği | Yakalanır, loglanır; maç geçersiz ve puansız; `room_panic` sayacı | 2, 5 |
| İlk yarı avcısı | İnsan-insan: oda kimliğinden tohumlu rastgele. Bot maçı: insan KAÇAK | 7 |
| Rol sırası telemetrisi | İlk yarı avcısının kazanma oranı ölçülür | 5 |
| Ölçüm planı | Opaque relay öncesi/sonrası ayrı satır | 9 |
| Hız toleransı | Paket başına `B_jitter` yerine zamanla dolan slack bütçesi (`S_max`); mesafe adımı alttan kırpılmaz | 1.2, 8 |
| Grace sayacı | Oda başına değil oyuncu başına | 2 |
| Dönüş sınırı | `Δt_min` tabanı tamamen kaldırıldı; ayrı dönüş kredisi yok; `ε_hareket` altında `θ` güncellenmez | 1.2, 8 |
| Spawn sonrası ilk saniye payı | Olduğu gibi kalır; bilinen sınır | 11 |
| Toplam grace | Oyuncu başına maç toplamı `T_grace_total` = 15 sn | 2, 8 |
| Bot kaçak duvarı | Dönüş yarıçapı (`V_kaçak / ω_max` + 15) kadar önce döner; adımını hattın `Δt`'siyle planlar; tam maçta `bot_clamp == 0` | 1.5, 8 |

