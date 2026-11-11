// Package buildinfo 只放「这版是哪个版本」这一件事。
//
// 为什么单独存在：Go 版没有 FastAPI 的 /openapi.json 自动暴露版本，
// 线上出问题时只能靠 md5 比对二进制才能确定跑的是哪一版。
// Python 版已经踩过这个坑（部署完自报 0.4.0，代码却是 0.5.0），
// 这里提前堵上：版本号进 /healthz，一个 curl 就能回答「线上跑的到底是哪版」。
//
// 发版时只改这一处；它与 git tag 必须同名。
package buildinfo

// Version 与 Python 版的发布号对齐（当前 Python v0.5.0）。
const Version = "0.5.0"
