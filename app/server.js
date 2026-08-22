const express = require('express');
const os = require('os');
const app = express();

app.get('/', (req, res) => {
  res.send(`Merhaba! Bu yanıtı veren konteyner: ${os.hostname()}\n`);
});

app.get('/healthz', (req, res) => res.send('ok\n'));

app.listen(3000, () => console.log('3000 portunda dinliyorum'));
