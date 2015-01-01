package main

import (
	"fmt"
	"io/ioutil"
)

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
