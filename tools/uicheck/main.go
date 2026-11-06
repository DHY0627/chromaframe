// Command uicheck drives the Chromaframe UI in headless Chromium to verify the
// preview iframe, the scrubber and playback without showing a window.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/chromedp/chromedp"
)

func main() {
	base := flag.String("url", "http://127.0.0.1:47821/", "UI URL")
	token := flag.String("token", "", "UI token")
	shot := flag.String("shot", "", "optional screenshot path")
	flag.Parse()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", "new"),
		chromedp.WindowSize(1480, 960),
		chromedp.Flag("hide-scrollbars", true),
	)
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel2 := chromedp.NewContext(allocCtx)
	defer cancel2()
	ctx, cancel3 := context.WithTimeout(ctx, 60*time.Second)
	defer cancel3()

	uiURL := *base + "?token=" + *token
	var entered string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(uiURL),
		chromedp.Sleep(2500*time.Millisecond),
		chromedp.Evaluate(`document.querySelector('#htmlPath').value`, &entered),
	); err != nil {
		fail(err)
	}
	fmt.Println("htmlPath =", entered)

	var boxes map[string]any
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){
	  var ids=['renderFrame','frameBadge','renderScan','preview','canvasFrame'];
	  var o={};
	  ids.forEach(function(id){
	    var el=document.getElementById(id); if(!el){o[id]=null;return;}
	    var r=el.getBoundingClientRect(); var cs=getComputedStyle(el);
	    o[id]={hidden:el.hidden, display:cs.display, opacity:cs.opacity, x:r.x, y:r.y, w:r.width, h:r.height, src:el.getAttribute('src')};
	  });
	  return o;
	})()`, &boxes)); err != nil {
		fail(err)
	}
	fmt.Printf("boxes = %#v\n", boxes)

	var msgs int
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`(function(){window.__cfmsgs=[];window.addEventListener('message',function(e){if(e.data&&e.data.source==='chromaframe'){window.__cfmsgs.push(e.data)}});return 1})()`, nil),
	); err != nil {
		fail(err)
	}

	var slider string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`(function(){var el=document.querySelector('#scrub');el.value='5000';el.dispatchEvent(new Event('input',{bubbles:true}));return el.value})()`, &slider),
		chromedp.Sleep(1200*time.Millisecond),
		chromedp.Evaluate(`window.__cfmsgs.length`, &msgs),
	); err != nil {
		fail(err)
	}
	fmt.Println("scrub value =", slider, "preview messages =", msgs)

	var cur, total string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.querySelector('#curTime').textContent`, &cur),
		chromedp.Evaluate(`document.querySelector('#totalTime').textContent`, &total),
	); err != nil {
		fail(err)
	}
	fmt.Println("timeline =", cur, "/", total)

	var before, after string
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.querySelector('#curTime').textContent`, &before),
		chromedp.Click(`#playBtn`, chromedp.ByID),
		chromedp.Sleep(900*time.Millisecond),
		chromedp.Evaluate(`document.querySelector('#curTime').textContent`, &after),
	); err != nil {
		fail(err)
	}
	fmt.Println("play:", before, "->", after)
	if before == after {
		fail(fmt.Errorf("playback did not advance the timeline"))
	}

	if *shot != "" {
		var buf []byte
		if err := chromedp.Run(ctx, chromedp.CaptureScreenshot(&buf)); err != nil {
			fail(err)
		}
		if err := os.WriteFile(*shot, buf, 0o644); err != nil {
			fail(err)
		}
		fmt.Println("screenshot =", *shot)
	}
	fmt.Println("UI CHECK OK")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "UI CHECK FAILED:", err)
	os.Exit(1)
}
