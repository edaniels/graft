# Slogor - A colorful slog handler

Slogor is a Go package designed to enhance logging capabilities. It provides functionalities for configuring the console mode and defining ANSI color codes, allowing developers to create more visually appealing and informative logs within their applications. With Slogor, users can improve the readability and presentation of their logs, making the debugging and monitoring process more efficient and intuitive.

![slogor demo](demo.png)

## ✨ Features
- 🚀 Fast and memory efficient
- 🧑‍💻 Standard logging with [slog](https://pkg.go.dev/log/slog) and close to official `TextHandler`
- 🎉 Concurrent/Thread-safe
- 🌈 [ANSI](https://en.wikipedia.org/wiki/ANSI_escape_code#3-bit_and_4-bit) colored with automatic Windows support
- ✅ Dependency free (except under Windows)
- ♻️ Customizable writer, logging level and time format
- 🔧 Debug with the caller path

## ❓ How to use
```golang
slog.SetDefault(slog.New(slogor.NewHandler(os.Stderr, slogor.SetLevel(slog.LevelDebug), slogor.SetTimeFormat(time.Stamp), slogor.ShowSource())))
slog.Info("I'm an information message, everything's fine")
```
- Minimal example: https://gitlab.com/greyxor/slogor/-/snippets/3612780
- Complete example: https://gitlab.com/greyxor/slogor/-/snippets/3611844

## ❓ How to hide time ?
Give an empty time format(`TimeFormat`) to not display the time

## 👷 [Thanks to contributors](https://gitlab.com/greyxor/slogor/-/graphs/main)