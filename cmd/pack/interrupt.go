package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// interrupts are the saga's two stops. The first interrupt (Ctrl-C, or
// SIGTERM) is done to soft and says first; the second to hard and says
// second — after it the signals are pack's no more, and a third ends the
// process as it would any program. With no second, the second interrupt
// ends it. stop releases them.
func interrupts(first, second string) (soft, hard context.Context, stop func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	soft, cancelSoft := context.WithCancel(context.Background())
	hard, cancelHard := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		for i, say := range []string{first, second} {
			select {
			case <-ch:
			case <-done:
				return
			}
			_, _ = fmt.Fprintln(os.Stderr, say)
			if i == 0 {
				cancelSoft()
				if second == "" {
					signal.Stop(ch)
					return
				}
			} else {
				signal.Stop(ch)
				cancelHard()
			}
		}
	}()
	return soft, hard, func() {
		signal.Stop(ch)
		close(done)
		cancelSoft()
		cancelHard()
	}
}

const (
	sayRollingBack = "interrupted: the step under way is cut short and its unit rolled back — interrupt again to stop at once"
	sayStopping    = "interrupted again: stopping at once — the record says how far the saga got"
)
