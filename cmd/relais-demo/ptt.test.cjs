const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const {Controller,classify,definite,retryable}=require('./web/ptt.js');
function rig(reply,deadlineMs=15000) {
 let time=0,n=0;const requests=[],notices=[];
 const c=new Controller({path:'/calls/test/agent/control',id:()=>`id-${++n}`,now:()=>time,sleep:async(ms)=>{time+=ms;},notice:(s)=>notices.push(s),deadlineMs,request:async(path,options)=>{requests.push({path,...options});return reply(JSON.parse(options.body),requests.length);}});
 return {c,requests,notices};
}
const response=(status,data)=>({status,text:async()=>JSON.stringify(data)});
test('identical retry through 202, unknown errors, lost responses and durable receipt',async()=>{
 const {c,requests}=rig((m,n)=>{if(n===1)return response(202,{...m,checkpointed:false});if(n===2)throw Error('lost');if(n===3)return response(409,{code:'new_code'});return response(200,{...m,checkpointed:true});});
 await c.send('ptt.start');assert.equal(requests.length,4);assert.equal(new Set(requests.map(r=>r.body)).size,1);
});
test('stop can follow a locally accepted start while its checkpoint is pending',async()=>{
 let release;const gate=new Promise(resolve=>{release=resolve;});let starts=0;
 const {c,requests}=rig(async(m)=>{if(m.name==='ptt.start' && ++starts>1)await gate;if(m.name==='ptt.stop'){release();return response(200,{...m,checkpointed:true});}return response(starts===1?202:200,{...m,checkpointed:starts>1});});
 await Promise.all([c.send('ptt.start'),c.send('ptt.stop')]);
 assert.deepEqual([...new Set(requests.map(r=>JSON.parse(r.body).sequence))],[1,2]);
});
test('stale proves accepted, definite refuses, deadline freezes sequence',async()=>{
 const stale=rig(()=>response(409,{code:'control_stale'}));await stale.c.send('ptt.start');assert.equal(stale.requests.length,1);
 const refused=rig(()=>response(409,{code:'control_unsupported'}));await assert.rejects(refused.c.send('ptt.start'),/refused/);assert.equal(refused.requests.length,1);
 const unknown=rig(()=>response(500,{}),100);await assert.rejects(unknown.c.send('ptt.start'),/timed out/);await assert.rejects(unknown.c.send('ptt.start'));assert.equal(unknown.requests.length,2);
});
test('refusal classes cover the API table; unfamiliar codes are uncertain',()=>{
 const docs=fs.readFileSync('docs/api_reference.md','utf8');
 for(const [label,set] of [['retryable',retryable],['definite',definite]]) {const line=docs.split('\n').find(s=>s.startsWith(`| ${label} |`));const codes=[...line.matchAll(/`([^`]+)`/g)].map(m=>m[1]);for(const code of codes)assert.ok(set.has(code),code);assert.equal(set.size,codes.length);}
 assert.equal(classify('future_code'),'uncertain');assert.equal(classify('closed'),'retryable');
});
