// The "build": copy the server into dist/, as a compiler would.
const fs = require("fs");

fs.mkdirSync("dist", { recursive: true });
fs.copyFileSync("src/server.js", "dist/server.js");
