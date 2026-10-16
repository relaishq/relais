#!/usr/bin/env python3
"""Export bounded raw receipts plus independently derived receipt/audio checks."""
import argparse,base64,gzip,hashlib,json,pathlib,shutil,statistics
p=argparse.ArgumentParser();p.add_argument('--scratch',required=True);p.add_argument('--dest',default='experiments/nvwa-room/evidence');a=p.parse_args();s=pathlib.Path(a.scratch).resolve();dest=pathlib.Path(a.dest);dest.mkdir(parents=True,exist_ok=True)
def packets(path):
 b=path.read_bytes();i=0;out=set()
 while i<len(b):
  if b[i:i+4]!=b'OggS':raise ValueError('bad Ogg capture')
  count=b[i+26];size=sum(b[i+27:i+27+count]);start=i+27+count;payload=b[start:start+size];i=start+size
  if not payload.startswith((b'OpusHead',b'OpusTags')):out.add(payload)
 return out
tones=[packets(s/'tone-440.ogg'),packets(s/'tone-880.ogg')]
assert not tones[0]&tones[1], 'the peer/self audio references must be disjoint'
rows=[];manifest={};raw=dest/'raw';raw.mkdir(exist_ok=True)
for mode in ['normal','disconnect','worker-kill','lost-receipt']:
 for trial in range(1,4):
  name=f'{mode}-{trial}';path=s/'ws-matrix'/name/'result.json';d=json.loads(path.read_text());assert not d['errors'],(name,d['errors']);row={'run':name,'duration_ms':d['duration_ms'],'queue_bytes':d['room_metrics']['max_queue_bytes'],'queue_age_ms':d['room_metrics']['max_queue_age_ms'],'applied':d['room_metrics']['applied'],'dedup':d['room_metrics']['deduplicated'],'conflicts':d['room_metrics']['conflicts'],'dropped_receipts':d['room_metrics']['dropped_receipts'],'duplicate_side_effects':d['room_metrics']['applied']-1000,'players':[]}
  assert row['duplicate_side_effects']==0
  if mode=='worker-kill':
   topology=json.loads((path.parent/'topology.json').read_text());owned=[c['pid'] for c in topology['children'] if c['name']=='worker-1'];assert owned==[d['kill_pid']]
  assert len({x['session_id'] for player in d['players'] for x in player['audio_sessions']})==sum(len(player['audio_sessions']) for player in d['players'])
  for i,player in enumerate(d['players']):
   assert len(player['messages'])==1000 and player['missing']==0
   joined=[o['reply'] for o in player['observations'] if o['reply']['type']=='joined']
   assert all(o['reply']['room_epoch']==d['snapshot']['room_epoch'] for o in player['observations'])
   if mode in ['disconnect','worker-kill'] and i==0:
    assert [j['connection_generation'] for j in joined]==[1,2]
    recovered=[p for p in joined[1]['players'] if p['id']==player['player_id']][0]
    assert recovered['state_seq']==250 and recovered['action']=={'id':'wave','version':1,'start_tick':0},recovered
   else:assert len(joined)==1

   seen=set();last={};generation=0;duplicates=reordered=0
   for obs in player['observations']:
    r=obs['reply']
    if r['type']=='joined':generation=r['connection_generation']
    request=r.get('request_id')
    if not request:continue
    key=(generation,request);n=int(request[1:])
    if key in seen:duplicates+=1
    elif n<last.get(generation,0):reordered+=1
    seen.add(key);last[generation]=max(last.get(generation,0),n)
   verified_packets=0
   for session in player['audio_sessions']:
    for encoded in session.get('evidence_packets',[]):
     payload=base64.b64decode(encoded);assert payload in tones[1-i] and payload not in tones[i],(name,i,'bad peer packet');verified_packets+=1
   assert verified_packets>=8,(name,'missing raw audio specimens')
   wire=[m['bytes']+len(m['reply']['request_id'])-5 for m in player['messages']]
   row['players'].append({'id':player['player_id'],'rtt_mean_ms':player['rtt_mean_ms'],'rtt_max_ms':player['rtt_max_ms'],'state_age_max_ms':player['latest_state_age_max_ms'],'missing':player['missing'],'duplicate_receipts':duplicates,'reordered_receipts':reordered,'reconnects':player['reconnects'],'recovery_ms':player['recovery_ms'],'cut_to_recovered_ms':player['cut_to_recovered_ms'],'audio_gap_ms':player['audio_max_gap_ms'],'audio_received':sum(x['received'] for x in player['audio_sessions']),'audio_peer':sum(x['peer_payload'] for x in player['audio_sessions']),'audio_self':sum(x['self_payload'] for x in player['audio_sessions']),'audio_unknown':sum(x['unknown_payload'] for x in player['audio_sessions']),'verified_raw_peer_packets':verified_packets,'wire_bytes_min':min(wire),'wire_bytes_max':max(wire),'version':player['final_room_version']})
  payload=json.dumps(d,separators=(',',':')).encode();target=raw/(name+'.json.gz');target.write_bytes(gzip.compress(payload,mtime=0));manifest[str(target.relative_to(dest))]={'sha256':hashlib.sha256(target.read_bytes()).hexdigest(),'source':str(path),'uncompressed_bytes':len(payload)};rows.append(row)
for name in ['tone-440.ogg','tone-880.ogg','build-sha256.txt','runtime-source-sha256.json']:
 if (s/name).exists():shutil.copy2(s/name,dest/name)
pressure=s/'backpressure-verified/result.json'
if pressure.exists():
 target=raw/'backpressure.json.gz';target.write_bytes(gzip.compress(pressure.read_bytes(),mtime=0));manifest[str(target.relative_to(dest))]={'sha256':hashlib.sha256(target.read_bytes()).hexdigest(),'source':str(pressure)}
(dest/'summary.json').write_text(json.dumps(rows,indent=2)+'\n');(dest/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
lines=['| Run | RTT mean A/B ms | RTT max A/B ms | State age max A/B ms | Queue B / age ms | Recovery A ms | Audio gap A/B ms | Version |','| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |']
for r in rows:
 x,y=r['players'];pair=lambda k:f'{x[k]:.3f}/{y[k]:.3f}'
 lines.append(f'| {r["run"]} | {pair("rtt_mean_ms")} | {pair("rtt_max_ms")} | {pair("state_age_max_ms")} | {r["queue_bytes"]}/{r["queue_age_ms"]:.3f} | {x["recovery_ms"]:.3f} | {pair("audio_gap_ms")} | {x["version"]} |')
(dest/'results-table.md').write_text('\n'.join(lines)+'\n');print('\n'.join(lines))
