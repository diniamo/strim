# Strim

CLI tool to watch local media with others. Handles both streaming and synchronization.

To make strim as seamless as possible, libp2p's public bootstrap mesh is used to advertise phrases and establish connections. The advertisement part is unreliable and slow, but I as far as I can tell, there is nothing I can do about that without hosting my own relay server (which I might do in the future).

## Building / Running

`go build` or `go run` the `cmd/strim` package.

** Usage

```
strim -h/--help
strim host <mpv args...>
strim connect [phrase [mpv args...]]
```
