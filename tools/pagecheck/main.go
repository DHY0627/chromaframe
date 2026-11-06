// Command pagecheck opens the injected preview page in headless Chromium and
// reports whether the controller booted and applied the requested time.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func main() {
	url := flag.String("url", "", "preview URL")
	flag.Parse()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", "new"),
		chromedp.WindowSize(1920, 1080),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel2 := chromedp.NewContext(allocCtx)
	defer cancel2()
	ctx, cancel3 := context.WithTimeout(ctx, 45*time.Second)
	defer cancel3()

	var errs []string
	chromedp.ListenTarget(ctx, func(ev any) {
		if e, ok := ev.(*runtime.EventExceptionThrown); ok {
			errs = append(errs, e.ExceptionDetails.Text)
		}
	})
	if err := chromedp.Run(ctx,
		chromedp.Navigate(*url),
		chromedp.Sleep(2500*time.Millisecond),
	); err != nil {
		fail(err)
	}
	var out map[string]any
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){
	  var all = document.getAnimations ? document.getAnimations() : [];
	  var first = document.querySelector('.title, h1, .big, .word');
	  return {
	    cf: typeof window.__cf,
	    version: window.__cf ? window.__cf.version : null,
	    time: window.__cf && window.__cf.getTime ? window.__cf.getTime() : null,
	    anims: all.length,
	    paused: all.filter(function(a){return a.playState==='paused'}).length,
	    firstText: first ? first.textContent.slice(0,40) : null,
	    firstOpacity: first ? getComputedStyle(first).opacity : null,
	    ready: document.readyState
	  };
	})()`, &out)); err != nil {
		fail(err)
	}
	fmt.Printf("state: %#v\n", out)
	fmt.Println("exceptions:", errs)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "PAGE CHECK FAILED:", err)
	os.Exit(1)
}
