const express = require('express');
const os = require('os');
const app = express();

// Ayarlar ortam degiskeninden gelir — imaja gomulmez.
const ORTAM      = process.env.ORTAM      || 'bilinmiyor';
const LOG_SEVIYE = process.env.LOG_SEVIYE || 'info';
const KARSILAMA  = process.env.KARSILAMA  || 'Merhaba';
const DB_PAROLA  = process.env.DB_PAROLA  || '';

let saglikli = true;   // /kirilsin ile bozacagiz

app.get('/', (req, res) => {
  res.send(`${KARSILAMA}! Konteyner: ${os.hostname()} | ortam: ${ORTAM}\n`);
});

app.get('/config', (req, res) => {
  res.json({
    ortam: ORTAM,
    logSeviye: LOG_SEVIYE,
    // parolayi asla loglama/dondurme — sadece geldigini dogruluyoruz
    dbParolaGeldiMi: DB_PAROLA.length > 0,
    dbParolaUzunluk: DB_PAROLA.length,
  });
});

app.get('/healthz', (req, res) => {
  if (!saglikli) return res.status(500).send('BOZUK\n');
  res.send('ok\n');
});

// Liveness probe demosu: bu adrese girince uygulama "hasta" olur
app.get('/kirilsin', (req, res) => {
  saglikli = false;
  console.log('!!! saglik durumu BOZUK olarak isaretlendi');
  res.send('artik /healthz 500 donuyor\n');
});

const server = app.listen(3000, () =>
  console.log(`3000 portunda dinliyorum (ortam=${ORTAM})`));

// GRACEFUL SHUTDOWN — PID 1 oldugumuz icin sinyali elle yakalamak SART.
// Yoksa Kubernetes 30sn bekleyip SIGKILL ile zorla oldurur.
function kapat(sinyal) {
  console.log(`${sinyal} alindi, yeni baglanti kabul edilmiyor...`);
  server.close(() => {
    console.log('acik baglantilar bitti, cikiliyor');
    process.exit(0);
  });
  // takilirsa 10sn sonra yine de cik
  setTimeout(() => process.exit(0), 10000).unref();
}
process.on('SIGTERM', () => kapat('SIGTERM'));
process.on('SIGINT',  () => kapat('SIGINT'));
