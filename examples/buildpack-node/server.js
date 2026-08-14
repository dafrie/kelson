// A hello-world with a health endpoint and no build file of any kind.
//
// The Paketo Node.js buildpacks detect this app from package.json, install its
// (zero) dependencies and start it with the `start` script, which is what
// makes the `web` process type in the built image. PORT is set by the platform;
// 3000 matches spec.components[web].port in project.yaml.
const http = require("http");

const port = process.env.PORT || 3000;

http
  .createServer((req, res) => {
    if (req.url === "/healthz") {
      res.writeHead(200, { "Content-Type": "text/plain" });
      res.end("ok\n");
      return;
    }
    res.writeHead(200, { "Content-Type": "text/plain" });
    res.end("hello from a buildpack\n");
  })
  .listen(port, () => {
    console.log(`greeter listening on ${port}`);
  });
