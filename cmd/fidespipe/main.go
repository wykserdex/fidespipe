package main

import (
	"github.com/wykserdex/fidespipe/internal/cli"
	"github.com/wykserdex/fidespipe/internal/compat"
)

func main() {
	// Совместимость с прежними именами переменных окружения: перенос значений
	// под новые имена ДО того, как что-либо прочитает окружение.
	compat.Init()
	cli.Run()
}
