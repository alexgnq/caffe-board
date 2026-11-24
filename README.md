# ☕ caffe board

A coffee bar for the room. Order a cup for the coding agent you work with and see what everyone else is drinking. It's a web version of the table in [caffe](https://github.com/alexgnq/caffe).

Every cup remembers the pod that poured it, and each pod gets its own colour. Scale to a few replicas and reload, and you can watch the cluster share the work.

It's one Go binary with PostgreSQL behind it. Built for [Ship your app on European Kubernetes](https://github.com/ankraio/ship-on-european-kubernetes).

## Run it locally

You need Docker and Go:

```sh
docker run --name caffe-db -e POSTGRES_PASSWORD=guest -p 5432:5432 -d postgres:19-alpine
DATABASE_URL='postgres://postgres:guest@localhost:5432/postgres?sslmode=disable' go run .
```

Then open <http://localhost:8080>.

## Configuration

| Variable | Default | |
|---|---|---|
| `DATABASE_URL` | | Postgres connection string. If unset, the standard `PG*` variables are used. |
| `PORT` | `8080` | Port to listen on. |
| `POD_NAME` | hostname | Shown on the page and stamped on each cup. |
| `APP_VERSION` | `1.0.0` | Shown in the footer. |

`/healthz` answers `ok` once the database is reachable.

## License

[MIT](LICENSE)
