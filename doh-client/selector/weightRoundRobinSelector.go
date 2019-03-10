package selector

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type WeightRoundRobinSelector struct {
	upstreams []*Upstream // upstreamsInfo
	client    http.Client // http client to check the upstream
}

func NewWeightRoundRobinSelector(timeout time.Duration) *WeightRoundRobinSelector {
	return &WeightRoundRobinSelector{
		client: http.Client{Timeout: timeout},
	}
}

func (ws *WeightRoundRobinSelector) Add(url string, upstreamType UpstreamType, weight int32) (err error) {
	switch upstreamType {
	case Google:
		ws.upstreams = append(ws.upstreams, &Upstream{
			Type:            Google,
			URL:             url,
			RequestType:     "application/dns-json",
			weight:          weight,
			effectiveWeight: weight,
		})

	case IETF:
		ws.upstreams = append(ws.upstreams, &Upstream{
			Type:            IETF,
			URL:             url,
			RequestType:     "application/dns-message",
			weight:          weight,
			effectiveWeight: weight,
		})

	default:
		return errors.New("unknown upstream type")
	}

	return nil
}

// COW, avoid concurrent read write upstreams
func (ws *WeightRoundRobinSelector) StartEvaluate() {
	go func() {
		for {
			wg := sync.WaitGroup{}

			for i := range ws.upstreams {
				wg.Add(1)

				go func(i int) {
					upstreamURL := ws.upstreams[i].URL
					var acceptType string

					switch ws.upstreams[i].Type {
					case Google:
						upstreamURL += "?name=www.example.com&type=A"
						acceptType = "application/dns-json"

					case IETF:
						// www.example.com
						upstreamURL += "?dns=q80BAAABAAAAAAAAA3d3dwdleGFtcGxlA2NvbQAAAQAB"
						acceptType = "application/dns-message"
					}

					req, err := http.NewRequest(http.MethodGet, upstreamURL, nil)
					if err != nil {
						/*log.Println("upstream:", upstreamURL, "type:", typeMap[upstream.Type], "check failed:", err)
						continue*/

						// should I only log it? But if there is an error, I think when query the server will return error too
						panic("upstream: " + upstreamURL + " type: " + typeMap[ws.upstreams[i].Type] + " check failed: " + err.Error())
					}

					req.Header.Set("accept", acceptType)

					resp, err := ws.client.Do(req)
					if err != nil {
						// should I check error in detail?
						if atomic.AddInt32(&ws.upstreams[i].effectiveWeight, -10) < 0 {
							atomic.StoreInt32(&ws.upstreams[i].effectiveWeight, 0)
						}
						return
					}

					switch ws.upstreams[i].Type {
					case Google:
						checkGoogleResponse(resp, ws.upstreams[i])

					case IETF:
						checkIETFResponse(resp, ws.upstreams[i])
					}

					wg.Done()
				}(i)
			}

			wg.Wait()

			time.Sleep(15 * time.Second)
		}
	}()
}

// nginx wrr like
func (ws *WeightRoundRobinSelector) Get() *Upstream {
	var (
		total             int32
		bestUpstreamIndex = -1
	)

	for i := range ws.upstreams {
		effectiveWeight := atomic.LoadInt32(&ws.upstreams[i].effectiveWeight)
		ws.upstreams[i].currentWeight += effectiveWeight
		total += effectiveWeight

		if bestUpstreamIndex == -1 || ws.upstreams[i].currentWeight > ws.upstreams[bestUpstreamIndex].currentWeight {
			bestUpstreamIndex = i
		}
	}

	ws.upstreams[bestUpstreamIndex].currentWeight -= total

	return ws.upstreams[bestUpstreamIndex]
}

func (ws *WeightRoundRobinSelector) ReportUpstreamStatus(upstream *Upstream, upstreamStatus upstreamStatus) {
	switch upstreamStatus {
	case Timeout:
		if atomic.AddInt32(&upstream.effectiveWeight, -10) < 0 {
			atomic.StoreInt32(&upstream.effectiveWeight, 0)
		}

	case Error:
		if atomic.AddInt32(&upstream.effectiveWeight, -5) < 0 {
			atomic.StoreInt32(&upstream.effectiveWeight, 0)
		}

	case OK:
		if atomic.AddInt32(&upstream.effectiveWeight, 2) > upstream.weight {
			atomic.StoreInt32(&upstream.effectiveWeight, upstream.weight)
		}
	}
}

func checkGoogleResponse(resp *http.Response, upstream *Upstream) {
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// server error
		if atomic.AddInt32(&upstream.effectiveWeight, -5) < 0 {
			atomic.StoreInt32(&upstream.effectiveWeight, 0)
		}
		return
	}

	m := make(map[string]interface{})
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		// should I check error in detail?
		if atomic.AddInt32(&upstream.effectiveWeight, -1) < 0 {
			atomic.StoreInt32(&upstream.effectiveWeight, 0)
		}
		return
	}

	if status, ok := m["status"]; ok {
		if statusNum, ok := status.(int); ok && statusNum == 0 {
			if atomic.AddInt32(&upstream.effectiveWeight, 5) > upstream.weight {
				atomic.StoreInt32(&upstream.effectiveWeight, upstream.weight)
			}
			return
		}
	}

	// should I check error in detail?
	if atomic.AddInt32(&upstream.effectiveWeight, -1) < 0 {
		atomic.StoreInt32(&upstream.effectiveWeight, 0)
	}
}

func checkIETFResponse(resp *http.Response, upstream *Upstream) {
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// server error
		if atomic.AddInt32(&upstream.effectiveWeight, -5) < 0 {
			atomic.StoreInt32(&upstream.effectiveWeight, 0)
		}
		return
	}

	if atomic.AddInt32(&upstream.effectiveWeight, 5) > upstream.weight {
		atomic.StoreInt32(&upstream.effectiveWeight, upstream.weight)
	}
}
