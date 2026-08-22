const express = require('express');
const os = require('os');
const { Pool } = require('pg');
const client = require('prom-client');
const app = express();

// --- Prometheus metrikleri ---
const kayit = new client.Registry();
kayit.setDefaultLabels({ uygulama: 'k8s-lab-app' });
client.collectDefaultMetrics({ register: kayit });   // cpu, bellek, event loop vb.

const istekSayaci = new client.Counter({
  name: 'http_istek_toplam',
  help: 'Toplam HTTP istegi',
  labelNames: ['yol', 'yontem', 'durum'],
  registers: [kayit],
});
const istekSuresi = new client.Histogram({
  name: 'http_istek_suresi_saniye',
  help: 'HTTP istek suresi',
  labelNames: ['yol', 'yontem'],
  buckets: [0.005, 0.01, 0.05, 0.1, 0.3, 0.5, 1, 2, 5],
  registers: [kayit],
});
const notSayaci = new client.Gauge({
  name: 'notlar_toplam',
  help: 'Veritabanindaki not sayisi',
  registers: [kayit],
});
app.use(express.json());

// --- Ayarlar: hepsi ortam degiskeninden, hicbiri kodda gomulu degil ---
const ORTAM      = process.env.ORTAM      || 'bilinmiyor';
const LOG_SEVIYE = process.env.LOG_SEVIYE || 'info';
const KARSILAMA  = process.env.KARSILAMA  || 'Merhaba';

const DB_HOST = process.env.DB_HOST || '';
const DB_ADI  = process.env.POSTGRES_DB || 'labdb';
const DB_KUL  = process.env.POSTGRES_USER || 'labuser';
const DB_PAR  = process.env.POSTGRES_PASSWORD || '';

// DB_HOST bossa veritabanisiz calis (chart'in dev modu icin)
const pool = DB_HOST
  ? new Pool({
      host: DB_HOST,
      database: DB_ADI,
      user: DB_KUL,
      password: DB_PAR,
      max: 5,                        // pod basina en fazla 5 baglanti
      connectionTimeoutMillis: 3000,
      idleTimeoutMillis: 30000,
    })
  : null;

// KRITIK: havuzdaki bosta bekleyen baglanti koparsa Pool 'error' yayar.
// Dinleyici olmazsa Node sureci komple cokertir (unhandled 'error' event).
if (pool) {
  pool.on('error', (err) => {
    console.error('pg havuz hatasi (yutuldu, surec yasamaya devam ediyor):', err.message);
  });
}

// Her istegi olc (metrik ucunun kendisi haric)
app.use((req, res, next) => {
  if (req.path === '/metrics') return next();
  const bitir = istekSuresi.startTimer({ yol: req.path, yontem: req.method });
  res.on('finish', () => {
    bitir();
    istekSayaci.inc({ yol: req.path, yontem: req.method, durum: res.statusCode });
  });
  next();
});

// Prometheus'un kazidigi uc
app.get('/metrics', async (req, res) => {
  if (pool) {
    try {
      const r = await pool.query('SELECT count(*)::int AS n FROM notlar');
      notSayaci.set(r.rows[0].n);
    } catch (e) { /* DB yoksa metrigi guncelleme */ }
  }
  res.set('Content-Type', kayit.contentType);
  res.end(await kayit.metrics());
});

app.get('/', (req, res) => {
  res.send(`${KARSILAMA}! Konteyner: ${os.hostname()} | ortam: ${ORTAM}\n`);
});

app.get('/config', (req, res) => {
  res.json({
    ortam: ORTAM,
    logSeviye: LOG_SEVIYE,
    dbBagli: Boolean(pool),
    dbHost: DB_HOST || null,
  });
});

// --- Veritabani uclari ---
app.get('/notlar', async (req, res) => {
  if (!pool) return res.status(503).json({ hata: 'veritabani yapilandirilmamis' });
  try {
    const r = await pool.query('SELECT id, metin, zaman FROM notlar ORDER BY id DESC LIMIT 20');
    res.json({ konteyner: os.hostname(), adet: r.rowCount, notlar: r.rows });
  } catch (e) {
    res.status(500).json({ hata: e.message });
  }
});

app.post('/notlar', async (req, res) => {
  if (!pool) return res.status(503).json({ hata: 'veritabani yapilandirilmamis' });
  const metin = (req.body && req.body.metin) || '';
  if (!metin) return res.status(400).json({ hata: 'metin alani gerekli' });
  try {
    const r = await pool.query(
      'INSERT INTO notlar (metin) VALUES ($1) RETURNING id, metin, zaman',  // parametreli sorgu — SQL injection'a kapali
      [metin]
    );
    res.status(201).json({ konteyner: os.hostname(), not: r.rows[0] });
  } catch (e) {
    res.status(500).json({ hata: e.message });
  }
});

// --- Saglik uclari ---
// readiness: veritabanina da bakar. DB yoksa trafik alma.
app.get('/readyz', async (req, res) => {
  if (!pool) return res.send('ok (dbsiz)\n');
  try {
    await pool.query('SELECT 1');
    res.send('ok\n');
  } catch (e) {
    res.status(503).send(`db erisilemiyor: ${e.message}\n`);
  }
});

// liveness: SADECE surecin canli olduguna bakar. DB'ye BAKMAZ.
app.get('/healthz', (req, res) => res.send('ok\n'));

// Yalnizca DEMO_UCLARI=true iken acilir (HPA demosu icin).
// Varsayilan KAPALI — disaridan CPU yakilmasini engeller.
if (process.env.DEMO_UCLARI === 'true') {
  app.get('/yuk', (req, res) => {
    const ms = Math.min(parseInt(req.query.ms || '200', 10), 2000);
    const bitis = Date.now() + ms;
    let x = 0;
    while (Date.now() < bitis) { x += Math.sqrt(Math.random()); }
    res.send(`${ms}ms cpu yakildi (${os.hostname()})\n`);
  });
  console.warn('UYARI: /yuk demo ucu ACIK (DEMO_UCLARI=true)');
}

const server = app.listen(3000, () =>
  console.log(`3000 portunda dinliyorum (ortam=${ORTAM}, db=${DB_HOST || 'yok'})`));

function kapat(sinyal) {
  console.log(`${sinyal} alindi, yeni baglanti kabul edilmiyor...`);
  server.close(async () => {
    if (pool) await pool.end();          // DB havuzunu da duzgun kapat
    console.log('acik baglantilar bitti, cikiliyor');
    process.exit(0);
  });
  setTimeout(() => process.exit(0), 10000).unref();
}
process.on('SIGTERM', () => kapat('SIGTERM'));
process.on('SIGINT',  () => kapat('SIGINT'));
