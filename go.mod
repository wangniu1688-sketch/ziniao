// 模块路径使用短名，便于 monorepo 内部引用；对外发布前改为规范域名路径即可（见 README）。
module ziniao

go 1.22

require (
	github.com/jackc/pgx/v5 v5.5.5
	github.com/refraction-networking/utls v1.6.7
)

require (
	github.com/andybalholm/brotli v1.0.6 // indirect
	github.com/cloudflare/circl v1.3.7 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20221227161230-091c0ba34f0a // indirect
	github.com/jackc/puddle/v2 v2.2.1 // indirect
	github.com/klauspost/compress v1.17.4 // indirect
	golang.org/x/crypto v0.21.0 // indirect
	golang.org/x/sync v0.1.0 // indirect
	golang.org/x/sys v0.18.0 // indirect
	golang.org/x/text v0.14.0 // indirect
)
