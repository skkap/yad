package main

import "syscall"

const termiosGet, termiosSet = syscall.TIOCGETA, syscall.TIOCSETA
