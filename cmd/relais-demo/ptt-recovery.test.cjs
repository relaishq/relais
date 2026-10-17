const {test}=require('node:test');
const assert=require('node:assert/strict');
const {Controller}=require('./web/ptt.js');
const response=(status,data)=>({status,text:async()=>JSON.stringify(data)});
function rig(request,options={}) {
 let time=0,n=0;const notices=[];
 const c=new Controller({path:'/p',id:()=>`id-${++n}`,now:()=>time,sleep:async ms=>{time+=ms;await new Promise(r=>setImmediate(r));},notice:s=>notices.push(s),request,...options});
 return {c,notices,time:()=>time};
}
test('takeover sequence gap retries while the earlier locally accepted message is unconfirmed',async()=>{
 let high=0,crashed=false,order=false;const confirmed=new Set(),bodies=new Map();
 const {c}=rig(async(_,o)=>{
  const m=JSON.parse(o.body);assert.equal(bodies.get(m.sequence) || o.body,o.body);bodies.set(m.sequence,o.body);
  if(m.sequence===1 && crashed && !order)return response(503,{code:'transient'});
  if(m.sequence===high+1){high=m.sequence;
   if(m.sequence===2 && !crashed){crashed=true;high=0;return response(202,{...m,checkpointed:false});}
   if(!crashed)return response(202,{...m,checkpointed:false});
  }
  if(m.sequence>high){order=true;return response(409,{code:'control_order'});}
  confirmed.add(m.sequence);return response(200,{...m,checkpointed:true});
 });
 await Promise.all([c.send('ptt.start'),c.send('ptt.stop')]);
 assert.equal(order,true);assert.deepEqual([...confirmed].sort(),[1,2]);assert.equal(c.failed,false);
});
test('sequence gap with no earlier unconfirmed message is definite',async()=>{
 let calls=0;const {c}=rig(async()=>{calls++;return response(409,{code:'control_order'});});
 await assert.rejects(c.send('ptt.start'),/control_order/);assert.equal(calls,1);
});
test('accepted press survives a 30 second store outage',async()=>{
 let first=true;const r=rig(async(_,o)=>{const m=JSON.parse(o.body);if(first){first=false;return response(202,{...m,checkpointed:false});}return r.time()<31000?response(503,{code:'transient'}):response(200,{...m,checkpointed:true});});
 await r.c.send('ptt.start');assert.ok(r.time()>=31000);assert.equal(r.c.failed,false);
});
test('unaccepted refusals reuse the sequence and leave the next press available',async()=>{
 let refused=true;const seen=[];const {c,notices}=rig(async(_,o)=>{const m=JSON.parse(o.body);seen.push(m);return refused?response(409,{code:'agent_capacity'}):response(200,{...m,checkpointed:true});},{deadlineMs:100});
 await assert.rejects(c.send('ptt.start'),/not accepted/i);assert.equal(c.failed,false);
 refused=false;await c.send('ptt.start');assert.equal(seen.at(-1).sequence,1);assert.notEqual(seen[0].id,seen.at(-1).id);assert.match(notices[0],/not accepted/i);
});
for(const status of [401,403])test(`HTTP ${status} stops immediately with launch-link instructions`,async()=>{
 let calls=0;const {c}=rig(async()=>{calls++;return response(status,{});});
 await assert.rejects(c.send('ptt.start'),/open the link printed by relais-demo/i);assert.equal(calls,1);
});
