package main

import "C"

//export answer
func answer() C.int { return C.int(42) }

func main() {}
