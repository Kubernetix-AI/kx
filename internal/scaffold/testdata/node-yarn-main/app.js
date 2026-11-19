const http = require("http");

const port = Number(process.env.PORT || 8080);
http
  .createServer((req, res) => res.end("node-yarn-main\n"))
  .listen(port, process.env.HOST || "0.0.0.0");
