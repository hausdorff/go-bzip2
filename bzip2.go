package main

import (
	"fmt"
	"io/ioutil"
)

func updateCrc(crcVar uint32, cha byte) uint32 {
	crcVar = (crcVar << 8) ^ CrcTable[(crcVar >> 24) ^ uint32(cha)]
	return crcVar
}

func finalizeCrc(crcVar uint32) uint32 {
	crcVar = ^(crcVar)
	return crcVar
}

func doCrc(text []byte) uint32 {
	crc := CrcInitVal
	for _, b := range text {
		crc = updateCrc(crc, b)
	}
	return crc
}

func main() {
	data, _ := ioutil.ReadFile("data/gutenburg20.txt")
	text := string(data)

	crc := doCrc(data)
	crc = finalizeCrc(crc)

	fmt.Println(text)
	fmt.Printf("0x%x\n", crc)
}
