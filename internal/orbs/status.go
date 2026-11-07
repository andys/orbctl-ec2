package orbs

import (
	"context"
	"html/template"
	"net/http"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

var statusPage = template.Must(template.New("status").Funcs(template.FuncMap{
	"tag":   tagValue,
	"state": orbState,
	"age": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return time.Since(*t).Round(time.Second).String()
	},
}).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta http-equiv="refresh" content="10">
<title>orb-ec2 · {{.Provider}}</title>
<style>
body { font: 14px/1.4 system-ui, sans-serif; margin: 2em; color: #222; }
table { border-collapse: collapse; margin-bottom: 2em; }
th, td { text-align: left; padding: .35em .9em; border-bottom: 1px solid #ddd; }
th { background: #f4f4f4; }
code { font-size: 13px; }
.state { font-weight: 600; }
.running { color: #18794e; } .hibernated { color: #6e56cf; } .stopped { color: #8a6d00; }
.pending, .stopping, .shutting-down { color: #c2410c; } .terminated { color: #999; }
.error { color: #b91c1c; }
</style></head><body>
<h1>orb-ec2 provider <code>{{.Provider}}</code> in <code>{{.Region}}</code></h1>
<p>{{.Now.Format "2006-01-02 15:04:05 MST"}} · refreshes every 10 seconds</p>
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
<h2>Instances</h2>
{{if .Instances}}<table>
<tr><th>Orb</th><th>Instance</th><th>State</th><th>Type</th><th>Zone</th><th>Private IP</th><th>Agent</th><th>Launched</th><th>Reason</th></tr>
{{range .Instances}}{{$state := state .}}<tr>
<td>{{with tag .Tags "amp-orb-id"}}{{.}}{{else}}{{tag .Tags "Name"}}{{end}}</td>
<td><code>{{.InstanceId}}</code></td>
<td class="state {{$state}}">{{$state}}</td>
<td>{{tag .Tags "amp-orb-type"}} {{.InstanceType}}</td>
<td>{{.Placement.AvailabilityZone}}</td>
<td>{{.PrivateIpAddress}}</td>
<td>{{if tag .Tags "amp-orb-pubkey"}}key published{{else}}booting{{end}}</td>
<td title="{{.LaunchTime}}">{{age .LaunchTime}} ago</td>
<td>{{.StateTransitionReason}}</td>
</tr>{{end}}
</table>{{else}}<p>No instances.</p>{{end}}
<h2>AMIs</h2>
{{if .Images}}<table>
<tr><th>AMI</th><th>Kind</th><th>State</th><th>Arch</th><th>Created</th><th>Image / snapshot</th></tr>
{{range .Images}}<tr>
<td><code>{{.ImageId}}</code></td>
<td>{{tag .Tags "amp-orb-kind"}}</td>
<td>{{.State}}</td>
<td>{{tag .Tags "amp-orb-arch"}}</td>
<td>{{.CreationDate}}</td>
<td>{{with tag .Tags "amp-orb-snapshot-id"}}{{.}}{{else}}{{tag .Tags "amp-orb-image"}}{{end}}</td>
</tr>{{end}}
</table>{{else}}<p>No AMIs tagged <code>amp-orb-provider={{.Provider}}</code> in {{.Region}}.</p>
{{if .Others}}<p class="error">These AMIs look like ours but belong to another provider name (set ORB_EC2_PROVIDER to match):</p>
<table><tr><th>AMI</th><th>Name</th><th>Provider tag</th><th>State</th><th>Created</th></tr>
{{range .Others}}<tr><td><code>{{.ImageId}}</code></td><td>{{.Name}}</td><td>{{tag .Tags "amp-orb-provider"}}</td><td>{{.State}}</td><td>{{.CreationDate}}</td></tr>{{end}}
</table>{{end}}{{end}}
</body></html>
`))

// Serve runs a web server that shows this provider's instances and AMIs,
// and prunes them every hour unless ORB_EC2_PRUNE_AFTER is off.
func (o *Orbs) Serve(addr string) error {
	http.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		data := struct {
			Provider  string
			Now       time.Time
			Instances []types.Instance
			Images    []types.Image
			Others    []types.Image // our AMIs under other provider names
			Region    string
			Error     error
		}{Provider: o.cfg.Provider, Region: o.region, Now: time.Now()}
		// Terminated instances stay visible in EC2 for about an hour.
		data.Instances, data.Error = o.describeInstances(ctx)
		sort.Slice(data.Instances, func(i, j int) bool {
			return aws.ToTime(data.Instances[i].LaunchTime).After(aws.ToTime(data.Instances[j].LaunchTime))
		})
		if data.Error == nil {
			data.Images, data.Error = o.describeImages(ctx, []string{"self"}, filter("tag:"+tagProvider, o.cfg.Provider))
		}
		if data.Error == nil && len(data.Images) == 0 {
			others, err := o.ownImages(ctx, "")
			if err != nil {
				logf("status page: %v", err)
			}
			data.Others = others
		}
		if data.Error != nil {
			logf("status page: %v", data.Error)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if data.Error != nil {
			w.WriteHeader(http.StatusBadGateway)
		}
		if err := statusPage.Execute(w, data); err != nil {
			logf("status page: %v", err)
		}
	})
	if o.cfg.PruneAfter > 0 {
		go o.pruneEvery(time.Hour)
	}
	logf("serving status for provider %s on %s", o.cfg.Provider, addr)
	return http.ListenAndServe(addr, nil)
}

// pruneEvery runs Prune with ORB_EC2_PRUNE_AFTER now and then at each interval.
func (o *Orbs) pruneEvery(interval time.Duration) {
	logf("pruning orbs and AMIs unused for %s every %s", o.cfg.PruneAfter, interval)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), interval)
		if err := o.Prune(ctx, o.cfg.PruneAfter, false); err != nil {
			logf("prune: %v", err)
		}
		cancel()
		time.Sleep(interval)
	}
}

// orbState is the instance's state, or "hibernated" for a hibernated stop.
func orbState(inst types.Instance) string {
	if hibernated(&inst) {
		return "hibernated"
	}
	return string(inst.State.Name)
}
