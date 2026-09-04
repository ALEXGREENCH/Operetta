package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"strings"

	"operetta/protocol/operamini4"
)

type visualScene struct {
	Scene  operamini4.Scene  `json:"scene"`
	Images map[string]string `json:"images,omitempty"`
}

type visualCase struct {
	Result    comparison   `json:"result"`
	Reference *visualScene `json:"reference,omitempty"`
	Native    *visualScene `json:"native,omitempty"`
}

func buildVisualScene(document *operamini4.ApplicationDocument) *visualScene {
	if document == nil {
		return nil
	}
	result := &visualScene{Scene: operamini4.BuildScene(document), Images: make(map[string]string)}
	for _, drawing := range document.Drawings {
		if drawing.Kind != 'I' {
			continue
		}
		data, mime := inlineImageAt(document.Page, drawing.ImagePointer)
		if len(data) == 0 || mime == "" {
			continue
		}
		digest := sha256.Sum256(data)
		key := fmt.Sprintf("sha256:%x", digest)
		if _, exists := result.Images[key]; !exists {
			result.Images[key] = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
		}
	}
	if len(result.Images) == 0 {
		result.Images = nil
	}
	return result
}

func inlineImageAt(page []byte, pointer int) ([]byte, string) {
	if pointer < 0 || pointer+2 > len(page) {
		return nil, ""
	}
	length := int(binary.BigEndian.Uint16(page[pointer : pointer+2]))
	if length <= 0 || pointer+2+length > len(page) {
		return nil, ""
	}
	data := page[pointer+2 : pointer+2+length]
	mime := ""
	switch {
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		mime = "image/jpeg"
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		mime = "image/png"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		mime = "image/gif"
	}
	return data, mime
}

func writeVisualReport(path string, report []comparison) error {
	cases := make([]visualCase, 0, len(report))
	for _, item := range report {
		cases = append(cases, visualCase{Result: item, Reference: item.referenceVisual, Native: item.nativeVisual})
	}
	data, err := json.Marshal(cases)
	if err != nil {
		return err
	}
	var output strings.Builder
	if err := visualReportTemplate.Execute(&output, struct{ JSON template.JS }{JSON: template.JS(data)}); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(output.String()), 0o600)
}

var visualReportTemplate = template.Must(template.New("om4-report").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Operetta OM4 visual comparison</title>
<style>
:root{color-scheme:dark;font:14px/1.45 system-ui,sans-serif;background:#11151b;color:#e7edf5}
*{box-sizing:border-box}body{margin:0}.top{position:sticky;top:0;z-index:50;padding:12px 18px;background:#151b23eF;border-bottom:1px solid #34404d;backdrop-filter:blur(8px)}
h1{font-size:18px;margin:0 0 7px}.controls{display:flex;gap:16px;align-items:center;color:#b9c5d3}.cases{padding:14px;display:grid;gap:18px}
.case{background:#1a212b;border:1px solid #34404d;border-radius:10px;overflow:hidden}.case-head{padding:12px 14px;border-bottom:1px solid #34404d}
.case-head h2{font-size:15px;margin:0 0 8px;overflow-wrap:anywhere}.metrics{display:flex;flex-wrap:wrap;gap:7px}.metric{padding:4px 8px;background:#252f3b;border-radius:5px;color:#cbd7e4}.metric strong{color:#fff}
.pair{display:grid;grid-template-columns:repeat(2,minmax(285px,1fr));gap:1px;background:#34404d}.side{min-width:0;background:#151b23;padding:10px}.side h3{font-size:13px;margin:0 0 7px;display:flex;justify-content:space-between}.error{padding:12px;color:#ffb4a8;white-space:pre-wrap}
.scroll{height:min(68vh,720px);overflow:auto;background:#0c1015;padding:12px;scrollbar-color:#516071 #171d25}.canvas{position:relative;margin:0 auto;transform-origin:top left;box-shadow:0 0 0 1px #526170;background:#fff;overflow:hidden}
.frag{position:absolute;overflow:hidden}.frag.text{white-space:pre-wrap;font:12px/14px Arial,sans-serif;z-index:3}.frag.image{object-fit:fill;z-index:4}.frag.image-missing{z-index:4;border:1px solid #8996a6;background:repeating-linear-gradient(135deg,#aab3bf 0 5px,#d4dae1 5px 10px);color:#26313d;font:8px/10px monospace}.frag.control{z-index:5;border:1px solid #667788;background:#fff}
.focus{position:absolute;z-index:10;border:1px dashed #00c853;background:#00c85312;opacity:0;pointer-events:none}.show-links .focus{opacity:1;pointer-events:auto}.focus:hover{background:#00e67640;border-style:solid}.missing{color:#f2c078}.extra{color:#8fd0ff}
details{padding:9px 14px;border-top:1px solid #2c3743}summary{cursor:pointer}.tokens{display:grid;grid-template-columns:1fr 1fr;gap:16px}.tokens code{white-space:pre-wrap;overflow-wrap:anywhere}
@media(max-width:760px){.pair{grid-template-columns:1fr}.scroll{height:520px}.tokens{grid-template-columns:1fr}}
</style>
</head>
<body>
<header class="top"><h1>Operetta OM4 visual comparison</h1><div class="controls"><label><input id="links" type="checkbox" checked> focus regions</label><span id="summary"></span></div></header>
<main class="cases" id="cases"></main>
<script>
const cases={{.JSON}};
const root=document.getElementById('cases');
const pct=n=>(100*(Number(n)||0)).toFixed(1)+'%';
const metric=(label,value)=>{const node=document.createElement('span');node.className='metric';const strong=document.createElement('strong');strong.textContent=value;node.append(label+' ',strong);return node};
function draw(bundle,label,error){
  const side=document.createElement('section');side.className='side';const title=document.createElement('h3');title.append(label);side.append(title);
  if(!bundle){const e=document.createElement('div');e.className='error';e.textContent=error||'scene unavailable';side.append(e);return side}
  const scene=bundle.scene;const meta=document.createElement('small');meta.textContent=scene.document.height+' px · '+scene.fragments.length+' fragments';title.append(meta);
  const scroll=document.createElement('div');scroll.className='scroll';const canvas=document.createElement('div');canvas.className='canvas';canvas.style.width=scene.viewport.width+'px';canvas.style.height=scene.document.height+'px';canvas.style.background=scene.document.background||'#fff';
  for(const f of scene.fragments){let node;if(f.kind==='background'){node=document.createElement('div');node.style.background=f.color||'#fff'}else if(f.kind==='text'){node=document.createElement('div');node.className='text';node.textContent=f.text||'';node.style.color=f.color||'#000';if(f.style&1)node.style.fontStyle='italic';if(f.style&2)node.style.fontWeight='bold'}else if(f.kind==='image'){const src=f.image&&bundle.images&&bundle.images[f.image.digest];if(src){node=document.createElement('img');node.className='image';node.src=src;node.alt=''}else{node=document.createElement('div');node.className='image-missing';node.textContent='IMG'}}else if(f.kind==='control'){node=document.createElement('div');node.className='control'}else if(f.kind==='link'){node=document.createElement('a');node.className='focus';node.href=f.link.target||'#';node.target='_blank';node.rel='noreferrer';node.title=f.link.target||''}else continue;
    node.classList.add('frag');node.style.left=f.x+'px';node.style.top=f.y+'px';node.style.width=Math.max(0,f.width)+'px';node.style.height=Math.max(0,f.height)+'px';canvas.append(node)
  }
  scroll.append(canvas);side.append(scroll);return side
}
function buildCase(item,index){const r=item.result;const section=document.createElement('article');section.className='case show-links';const head=document.createElement('header');head.className='case-head';const h=document.createElement('h2');const a=document.createElement('a');a.href=r.requested_url;a.target='_blank';a.rel='noreferrer';a.textContent=(index+1)+'. '+r.requested_url;h.append(a);head.append(h);const metrics=document.createElement('div');metrics.className='metrics';metrics.append(metric('native text',pct(r.reference_native_token_coverage)),metric('geometry',pct(r.reference_native_geometry_coverage)),metric('colors',pct(r.reference_native_color_coverage)),metric('links',r.reference_links+' / '+r.native_links),metric('height',r.reference_height+' / '+r.native_height),metric('time',r.reference_duration_ms+' / '+r.native_duration_ms+' ms'));head.append(metrics);section.append(head);
  const pair=document.createElement('div');pair.className='pair';const left=draw(item.reference,'Official OM4',r.reference_error);const right=draw(item.native,'Operetta',r.native_error);pair.append(left,right);section.append(pair);const scrolls=pair.querySelectorAll('.scroll');if(scrolls.length===2){let syncing=false;for(const [from,to] of [[scrolls[0],scrolls[1]],[scrolls[1],scrolls[0]]])from.addEventListener('scroll',()=>{if(syncing)return;syncing=true;to.scrollTop=from.scrollTop;to.scrollLeft=from.scrollLeft;requestAnimationFrame(()=>syncing=false)})}
  const details=document.createElement('details');const summary=document.createElement('summary');summary.textContent='Text/token differences';details.append(summary);const tokens=document.createElement('div');tokens.className='tokens';for(const [name,values,cls] of [['Missing from native',r.reference_tokens_missing_from_native,'missing'],['Extra in native',r.native_extra_tokens,'extra']]){const box=document.createElement('div');const b=document.createElement('b');b.textContent=name;const code=document.createElement('code');code.className=cls;code.textContent='\n'+(values||[]).join(' · ');box.append(b,code);tokens.append(box)}details.append(tokens);section.append(details);return section}
for(const [i,item] of cases.entries())root.append(buildCase(item,i));
document.getElementById('links').addEventListener('change',e=>document.querySelectorAll('.case').forEach(n=>n.classList.toggle('show-links',e.target.checked)));
document.getElementById('summary').textContent=cases.length+' case'+(cases.length===1?'':'s')+' · generated locally';
</script>
</body>
</html>`))
