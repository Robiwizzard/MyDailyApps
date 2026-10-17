package config

import "time"

func sleep(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
