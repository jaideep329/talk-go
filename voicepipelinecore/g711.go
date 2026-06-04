package voicepipelinecore

import "encoding/binary"

var g711ULawSegmentEnds = [...]int{
	0x00FF,
	0x01FF,
	0x03FF,
	0x07FF,
	0x0FFF,
	0x1FFF,
	0x3FFF,
	0x7FFF,
}

func pcm16BytesToPCMU(pcm []byte) []byte {
	if len(pcm)%2 != 0 {
		pcm = pcm[:len(pcm)-1]
	}
	out := make([]byte, len(pcm)/2)
	for i := range out {
		sample := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
		out[i] = linear16ToULaw(sample)
	}
	return out
}

func linear16ToULaw(sample int16) byte {
	const (
		bias = 0x84
		clip = 32635
	)
	pcm := int(sample)
	mask := 0xFF
	if pcm < 0 {
		pcm = -pcm
		mask = 0x7F
	}
	if pcm > clip {
		pcm = clip
	}
	pcm += bias

	segment := 0
	for ; segment < len(g711ULawSegmentEnds); segment++ {
		if pcm <= g711ULawSegmentEnds[segment] {
			break
		}
	}
	if segment >= len(g711ULawSegmentEnds) {
		return byte(0x7F ^ mask)
	}

	uval := (segment << 4) | ((pcm >> (segment + 3)) & 0x0F)
	return byte(uval ^ mask)
}
