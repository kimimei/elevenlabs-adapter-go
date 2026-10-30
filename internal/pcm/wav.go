// Package pcm 把裸 PCM 流包成标准 RIFF/WAVE 容器。
//
// 为什么需要：OpenAI 的 response_format="wav" 语义是**带容器的 WAV**，
// 而 ElevenLabs 的 pcm_24000 是裸流。直接透传会让多数播放器解不出来，
// 客户以为适配器坏了，实际是容器缺失。
package pcm

import "encoding/binary"

// HeaderSize 是标准 44 字节 WAV 头。
const HeaderSize = 44

// Encode 把 16-bit 单声道裸 PCM 包成 RIFF/WAVE。
func Encode(data []byte, rate int) []byte {
	out := make([]byte, HeaderSize+len(data))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+len(data)))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16) // fmt chunk 大小
	binary.LittleEndian.PutUint16(out[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(out[22:24], 1)  // 声道数
	binary.LittleEndian.PutUint32(out[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(rate*2)) // byte rate = rate * channels * bitsPerSample/8
	binary.LittleEndian.PutUint16(out[32:34], 2)              // block align
	binary.LittleEndian.PutUint16(out[34:36], 16)             // bits per sample
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(len(data)))
	copy(out[HeaderSize:], data)
	return out
}

// Silence 生成指定秒数的静音 WAV（mock 模式用，不消耗上游额度）。
func Silence(seconds float64, rate int) []byte {
	n := int(float64(rate) * seconds)
	if n < 0 {
		n = 0
	}
	return Encode(make([]byte, n*2), rate)
}
