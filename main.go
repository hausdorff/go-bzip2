package main

import (
	"bytes"
	"fmt"
	"os"
	//"io/ioutil"
)

func doCrc(text []byte) uint32 {
	crc := CrcInitVal
	for _, b := range text {
		crc = updateCrc(crc, b)
	}
	return crc
}

func main() {
	//data, _ := ioutil.ReadFile("data/gutenburg20.txt")

	//crc := doCrc(data)
	//crc = finalizeCrc(crc)

	//fmt.Printf("0x%x\n", crc)

	//f, e := os.Open("data/gutenburg20.txt.bz2")
	f, e := os.Open("data/test1.bz2")
	if e == nil {
		bz := NewReader(f)
		buf := new(bytes.Buffer)
		buf.ReadFrom(bz)
		s := buf.String()
		fmt.Println(len(s))
	} else {
		fmt.Println(e)
		os.Exit(1)
	}
}
