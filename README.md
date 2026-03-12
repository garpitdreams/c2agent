example of use :
```
package main
 
import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
 
	"github.com/garpitdreams/c2agent"
)
 
func main() {
	agent := c2agent.New(c2agent.Config{
		ServerURL:    "https://local.gamecp.id",
		ServerWS:     "wss://local.gamecp.id",
		PollInterval: 5 * time.Minute,
		IdleTimeout:  10 * time.Minute,
	})
 
	if err := agent.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start agent: %v\n", err)
		os.Exit(1)
	}
 
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
 
	agent.Stop()
}

 ```
