# ☕ caffe board

[caffe](https://github.com/alexgnq/caffe) on the web. Every terminal running caffe with this board's address shows its table here the way it looks in the terminal: the same cards with the same lines, the tally, and the barista cat, coat and all, or the capybara on its turtle. When a session is ready for you, the cat runs under its card and calls it out.

The pods serving the page are listed as baristas, with how many caffe updates each one took. Scale to a few replicas and you can watch them share the work.

It's one Go binary with PostgreSQL behind it. Built for [Ship your app on European Kubernetes](https://github.com/ankraio/ship-on-european-kubernetes).

## Put your bar on it

```sh
CAFFE_BOARD_URL=https://your-board.example caffe
```

caffe sends its bar every 10 seconds and closes it when you quit. A bar that stops reporting closes after a minute. Project names and session titles stay on the Mac unless caffe runs with `CAFFE_BOARD_PROJECTS=1`.

## Run it locally

You need Docker and Go:

```sh
docker run --name caffe-db -e POSTGRES_PASSWORD=guest -p 5432:5432 -d postgres:19-alpine
DATABASE_URL='postgres://postgres:guest@localhost:5432/postgres?sslmode=disable' go run .
```

Then open <http://localhost:8080> and start caffe with `CAFFE_BOARD_URL=http://localhost:8080`.

## Configuration

| Variable | Default | |
|---|---|---|
| `DATABASE_URL` | | Postgres connection string. If unset, the standard `PG*` variables are used. |
| `PORT` | `8080` | Port to listen on. |
| `POD_NAME` | hostname | The barista name shown for this pod. |
| `BOARD_TOKEN` | | If set, caffe must send it as a bearer token to update a bar. |
| `APP_VERSION` | `1.0.0` | Shown in the footer. |

`/healthz` answers `ok` once the database is reachable.

## API

caffe uses two calls. `PUT /api/bars/{name}` replaces a bar with caffe's snapshot, and `DELETE /api/bars/{name}` closes it. Both take `Authorization: Bearer …` when `BOARD_TOKEN` is set.

```json
{
  "cat": {"kind": "cat", "name": "Miso", "title": "Miso the calico cat",
          "coat": "#f6f2e8", "shade": "#403834", "eyes": "#84be48", "shell": "#cc6254"},
  "tally": "1 brewing · 1 idle",
  "awake": 95,
  "cards": [
    {"id": "d5883a0d8b", "kind": "session", "agent": "claude", "name": "Claude", "where": "",
     "state": "brewing", "status": "brewing 1m", "note": "5% cpu"}
  ]
}
```

The cat's `kind` is `cat`, or `capybara` in caffe's capybara mode, where it rides a turtle with a `shell` of that colour and apps get boba tea. A card's `kind` is `session` or `app`. `agent` is one of `claude`, `codex`, `copilot`, `opencode`, `pi`, `vscode` or `t3`. `state` is one of `brewing`, `ready`, `idle`, `cold`, `pouring` or `served` for sessions, and `busy`, `open` or `closed` for apps.

## License

[MIT](LICENSE)
