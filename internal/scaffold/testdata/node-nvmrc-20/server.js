const http = require("http");

const port = Number(process.env.PORT || 8080);
http
  .createServer((req, res) => res.end("node-nvmrc-20\n"))
  .listen(port, process.env.HOST || "0.0.0.0");
