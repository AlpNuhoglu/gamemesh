# The embedded client

The gateway embeds this directory and serves the browser client from it
under `/play/`.

The client's build output is written to `app/` here (`npm run build` in
`web/`, or the Node stage of the Dockerfile) and is **not committed**. This
file is: it keeps the Go embed compiling on a checkout that has not built
the client. A gateway built without `app/index.html` has no client to serve.
