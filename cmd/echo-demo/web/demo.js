'use strict';

// Relais echo demo.
//
// The page publishes audio and video to one media worker with a single
// WHIP-style exchange (POST /calls with the SDP offer, 201 with the answer;
// DELETE the Location to hang up) and plays the echo the worker sends back.
//
//   ?source=camera   camera and microphone (the default; asks for permission)
//   ?source=test     an animated canvas and an oscillator; no permission prompt
//   ?autostart=0     wait for the Start button instead of calling on load
//
// For automation, the page keeps a stats summary in window.relaisStats and,
// as JSON, in the #stats element, refreshed every second. Any counter the
// browser does not expose is the string "unverified" and is listed in
// relaisStats.unverified; it is never reported as 0. window.relaisStart()
// and window.relaisHangup() control the call.

const UNVERIFIED = 'unverified';
const STATS_INTERVAL_MS = 1000;

const params = new URLSearchParams(location.search);
const source = params.get('source') === 'test' ? 'test' : 'camera';
const autostart = params.get('autostart') !== '0';

const els = {
  sourceLabel: document.getElementById('source-label'),
  start: document.getElementById('start'),
  hangup: document.getElementById('hangup'),
  unmute: document.getElementById('unmute'),
  local: document.getElementById('local'),
  remote: document.getElementById('remote'),
  summary: document.getElementById('summary'),
  stats: document.getElementById('stats'),
  log: document.getElementById('log'),
};

let pc = null;
let localStream = null;
let resourceURL = null;
let statsTimer = null;
const testSource = { timer: null, audioContext: null };

// Times are performance.now() values.
const call = {
  state: 'idle', // idle, starting, connecting, connected, failed, ended
  error: null,
  startedAt: null,
  connectedAt: null,
  firstEchoFrameAt: null,
  firstEchoFrameMethod: null,
  answer: null,
};

window.relaisStats = { source, callState: call.state, unverified: [] };
window.relaisStart = start;
window.relaisHangup = hangup;

function log(message) {
  const line = `${new Date().toISOString().slice(11, 23)}  ${message}`;
  els.log.textContent += `${line}\n`;
  console.log(`[relais] ${message}`);
}

function setCallState(state, error) {
  call.state = state;
  if (error) {
    call.error = String(error && error.message ? error.message : error);
    log(`error: ${call.error}`);
  }
  els.start.disabled = state === 'starting' || state === 'connecting' || state === 'connected';
  els.hangup.disabled = !pc;
  render(window.relaisStats);
}

// The test source: an animated canvas (moving square, clock, frame counter)
// and an oscillator sweeping around 440 Hz.
async function testStream() {
  const canvas = document.createElement('canvas');
  canvas.width = 640;
  canvas.height = 480;
  const g = canvas.getContext('2d');
  let frame = 0;
  const draw = () => {
    frame += 1;
    const t = performance.now() / 1000;
    g.fillStyle = `hsl(${Math.floor(t * 40) % 360}, 55%, 35%)`;
    g.fillRect(0, 0, canvas.width, canvas.height);
    const x = (Math.sin(t * 1.3) * 0.5 + 0.5) * (canvas.width - 120);
    const y = (Math.cos(t * 0.9) * 0.5 + 0.5) * (canvas.height - 200) + 80;
    g.fillStyle = '#fff';
    g.fillRect(x, y, 120, 120);
    g.font = 'bold 30px system-ui, sans-serif';
    g.fillText('Relais test pattern', 24, 48);
    g.font = '24px ui-monospace, monospace';
    g.fillText(`${new Date().toISOString().slice(11, 23)}  #${frame}`, 24, canvas.height - 24);
  };
  draw();
  // A timer, not requestAnimationFrame, so frames keep coming when the tab
  // is not painted (background tabs still slow it down).
  testSource.timer = setInterval(draw, 1000 / 30);
  const [video] = canvas.captureStream(30).getVideoTracks();

  const audioContext = new AudioContext();
  testSource.audioContext = audioContext;
  const tone = audioContext.createOscillator();
  tone.frequency.value = 440;
  const sweep = audioContext.createOscillator();
  sweep.frequency.value = 0.5;
  const sweepDepth = audioContext.createGain();
  sweepDepth.gain.value = 120;
  sweep.connect(sweepDepth).connect(tone.frequency);
  const level = audioContext.createGain();
  level.gain.value = 0.2;
  const destination = audioContext.createMediaStreamDestination();
  tone.connect(level).connect(destination);
  tone.start();
  sweep.start();
  // Without a user gesture Chrome keeps the context suspended and no audio
  // flows; resume() then settles only after a click, so it is not awaited.
  audioContext.resume().catch(() => {});
  if (audioContext.state !== 'running') {
    log('test audio waits for a click on the page (Chrome autoplay policy)');
  }

  return new MediaStream([...destination.stream.getAudioTracks(), video]);
}

function resumeTestAudio() {
  const ctx = testSource.audioContext;
  if (ctx && ctx.state === 'suspended') {
    ctx.resume().then(() => log('test audio running'), () => {});
  }
}
document.addEventListener('pointerdown', resumeTestAudio);
document.addEventListener('keydown', resumeTestAudio);

async function start() {
  if (pc) {
    return;
  }
  Object.assign(call, {
    error: null, startedAt: performance.now(), connectedAt: null,
    firstEchoFrameAt: null, firstEchoFrameMethod: null, answer: null,
  });
  setCallState('starting');

  try {
    localStream = source === 'test'
      ? await testStream()
      : await navigator.mediaDevices.getUserMedia({
        audio: true,
        video: { width: { ideal: 640 }, height: { ideal: 480 } },
      });
    els.local.srcObject = localStream;

    pc = new RTCPeerConnection({ bundlePolicy: 'max-bundle', rtcpMuxPolicy: 'require' });
    // Audio first, then video: one sendrecv m-line each, bundled.
    for (const kind of ['audio', 'video']) {
      const track = localStream.getTracks().find((t) => t.kind === kind);
      if (track) {
        pc.addTransceiver(track, { direction: 'sendrecv', streams: [localStream] });
      } else {
        log(`no local ${kind} track`);
      }
    }
    pc.ontrack = onTrack;
    pc.onconnectionstatechange = onConnectionStateChange;
    pc.oniceconnectionstatechange = () => log(`ICE connection state: ${pc.iceConnectionState}`);

    await pc.setLocalDescription(await pc.createOffer());
    // ICE-lite worker: it never checks the browser's candidates, so the
    // offer goes out without waiting for gathering.
    const response = await fetch('/calls', {
      method: 'POST',
      headers: { 'Content-Type': 'application/sdp' },
      body: pc.localDescription.sdp,
    });
    const body = await response.text();
    if (response.status !== 201) {
      throw new Error(`POST /calls: ${response.status} ${body.trim()}`);
    }
    resourceURL = response.headers.get('Location');
    call.answer = describeAnswer(body);
    log(`answer: ${JSON.stringify(call.answer)}`);
    await pc.setRemoteDescription({ type: 'answer', sdp: body });
    setCallState('connecting');

    statsTimer = setInterval(updateStats, STATS_INTERVAL_MS);
    updateStats();
  } catch (err) {
    teardown();
    setCallState('failed', err);
  }
}

function onConnectionStateChange() {
  const state = pc.connectionState;
  log(`connection state: ${state}`);
  if (state === 'connected' && call.connectedAt === null) {
    call.connectedAt = performance.now();
    setCallState('connected');
  } else if (state === 'failed' || state === 'closed') {
    setCallState('failed', `connection ${state}`);
  } else if (state === 'disconnected') {
    log('connection disconnected (may recover)');
  }
}

function onTrack(event) {
  const stream = event.streams[0] || new MediaStream([event.track]);
  if (els.remote.srcObject !== stream) {
    els.remote.srcObject = stream;
  }
  log(`echo ${event.track.kind} track arrived (mid ${event.transceiver.mid})`);
  if (event.track.kind === 'video' && 'requestVideoFrameCallback' in HTMLVideoElement.prototype) {
    els.remote.requestVideoFrameCallback((now) => {
      if (call.firstEchoFrameAt === null) {
        call.firstEchoFrameAt = now;
        call.firstEchoFrameMethod = 'requestVideoFrameCallback';
        log('first echoed video frame shown');
      }
    });
  }
}

// hangup ends the call. The last stats stay on the page.
async function hangup() {
  if (!pc) {
    return;
  }
  await updateStats();
  teardown();
  setCallState('ended');
}

// teardown releases the call: the worker's session (DELETE), the
// connection, the local media and the test source.
function teardown() {
  clearInterval(statsTimer);
  statsTimer = null;
  if (resourceURL) {
    fetch(resourceURL, { method: 'DELETE' }).catch((err) => log(`DELETE: ${err}`));
    resourceURL = null;
  }
  if (pc) {
    pc.close();
    pc = null;
  }
  for (const track of localStream ? localStream.getTracks() : []) {
    track.stop();
  }
  localStream = null;
  clearInterval(testSource.timer);
  testSource.timer = null;
  if (testSource.audioContext) {
    testSource.audioContext.close();
    testSource.audioContext = null;
  }
}

// describeAnswer reads what the worker answered: bundle, one codec per
// m-line and the host candidate (the IP the browser must reach).
function describeAnswer(sdp) {
  const answer = { bundle: null, media: [], candidates: [] };
  let media = null;
  for (const line of sdp.split(/\r?\n/)) {
    if (line.startsWith('a=group:BUNDLE ')) {
      answer.bundle = line.slice('a=group:BUNDLE '.length).split(' ');
    } else if (line.startsWith('m=')) {
      const [kind, port] = line.slice(2).split(' ');
      media = { kind, accepted: port !== '0', codecs: [] };
      answer.media.push(media);
    } else if (media && line.startsWith('a=rtpmap:')) {
      media.codecs.push(line.split(' ')[1]);
    } else if (line.startsWith('a=candidate:')) {
      answer.candidates.push(line.slice('a=candidate:'.length));
    }
  }
  return answer;
}

const DTLS_VERSIONS = { FEFF: 'DTLS 1.0', FEFD: 'DTLS 1.2', FEFC: 'DTLS 1.3' };

async function updateStats() {
  if (!pc) {
    return;
  }
  let report;
  try {
    report = await pc.getStats();
  } catch (err) {
    log(`getStats: ${err}`);
    return;
  }
  const byId = new Map();
  report.forEach((stat) => byId.set(stat.id, stat));
  const all = [...byId.values()];
  const unverified = [];

  // known returns a value, or "unverified" (and notes the path) when the
  // browser did not provide it; read does the same for a stat field.
  const known = (value, path) => {
    if (value === undefined || value === null) {
      unverified.push(path);
      return UNVERIFIED;
    }
    return value;
  };
  const read = (stat, field, path) => known(stat ? stat[field] : undefined, path);
  const pick = (stat, fields, prefix) => {
    const out = {};
    for (const field of fields) {
      out[field] = read(stat, field, `${prefix}.${field}`);
    }
    return out;
  };
  const codecOf = (stat, path) => {
    const codec = stat && stat.codecId ? byId.get(stat.codecId) : undefined;
    return read(codec, 'mimeType', `${path}.codec`);
  };

  const transport = all.find((s) => s.type === 'transport');
  const pair = (transport && byId.get(transport.selectedCandidatePairId))
    || all.find((s) => s.type === 'candidate-pair' && s.nominated && s.state === 'succeeded');
  const candidate = (id) => {
    const c = id ? byId.get(id) : undefined;
    return c ? `${c.address || c.ip}:${c.port} ${c.candidateType} ${c.protocol}` : undefined;
  };

  const rtp = (type, kind) => all.find((s) => s.type === type && s.kind === kind);
  const outbound = {};
  const inbound = {};
  const outboundFields = {
    audio: ['packetsSent', 'bytesSent'],
    video: ['packetsSent', 'bytesSent', 'framesEncoded', 'keyFramesEncoded', 'framesSent', 'frameWidth', 'frameHeight',
      'pliCount', 'firCount', 'nackCount', 'encoderImplementation', 'qualityLimitationReason'],
  };
  const inboundFields = {
    audio: ['packetsReceived', 'packetsLost', 'packetsDiscarded', 'bytesReceived', 'jitter', 'totalSamplesReceived',
      'concealedSamples'],
    video: ['packetsReceived', 'packetsLost', 'packetsDiscarded', 'bytesReceived', 'jitter', 'framesReceived',
      'framesDecoded', 'keyFramesDecoded', 'framesDropped', 'freezeCount', 'frameWidth', 'frameHeight', 'pliCount',
      'firCount', 'nackCount', 'decoderImplementation'],
  };
  for (const kind of ['audio', 'video']) {
    const out = rtp('outbound-rtp', kind);
    outbound[kind] = { codec: codecOf(out, `outbound.${kind}`), ...pick(out, outboundFields[kind], `outbound.${kind}`) };
    const inb = rtp('inbound-rtp', kind);
    inbound[kind] = { codec: codecOf(inb, `inbound.${kind}`), ...pick(inb, inboundFields[kind], `inbound.${kind}`) };
  }

  // Fallback for browsers without requestVideoFrameCallback: the first poll
  // that sees a decoded echo frame (accurate to one stats interval).
  if (call.firstEchoFrameAt === null && typeof inbound.video.framesDecoded === 'number' && inbound.video.framesDecoded > 0) {
    call.firstEchoFrameAt = performance.now();
    call.firstEchoFrameMethod = `getStats poll (within ${STATS_INTERVAL_MS} ms)`;
  }
  const between = (from, to, path) => known(from === null || to === null ? null : Math.round(to - from), path);

  // Chrome's getStats has no counter for SRTP/SRTCP packets it could not
  // decrypt, so this is never a number.
  unverified.push('srtpDecryptionFailures');

  const tlsVersion = read(transport, 'tlsVersion', 'transport.tlsVersion');
  const stats = {
    updatedAt: new Date().toISOString(),
    source,
    callState: call.state,
    error: call.error,
    connectionState: pc.connectionState,
    iceConnectionState: pc.iceConnectionState,
    iceGatheringState: pc.iceGatheringState,
    signalingState: pc.signalingState,
    transport: {
      dtlsState: read(transport, 'dtlsState', 'transport.dtlsState'),
      tlsVersion,
      dtlsVersion: DTLS_VERSIONS[String(tlsVersion).toUpperCase()] || tlsVersion,
      dtlsCipher: read(transport, 'dtlsCipher', 'transport.dtlsCipher'),
      srtpCipher: read(transport, 'srtpCipher', 'transport.srtpCipher'),
      dtlsRole: read(transport, 'dtlsRole', 'transport.dtlsRole'),
      iceRole: read(transport, 'iceRole', 'transport.iceRole'),
      iceState: read(transport, 'iceState', 'transport.iceState'),
      selectedCandidatePair: {
        local: known(candidate(pair && pair.localCandidateId), 'transport.selectedCandidatePair.local'),
        remote: known(candidate(pair && pair.remoteCandidateId), 'transport.selectedCandidatePair.remote'),
        currentRoundTripTime: read(pair, 'currentRoundTripTime', 'transport.selectedCandidatePair.currentRoundTripTime'),
      },
    },
    outbound,
    inbound,
    srtpDecryptionFailures: UNVERIFIED,
    timing: {
      connectedMs: between(call.startedAt, call.connectedAt, 'timing.connectedMs'),
      firstEchoFrameMs: between(call.startedAt, call.firstEchoFrameAt, 'timing.firstEchoFrameMs'),
      firstEchoFrameAfterConnectedMs: between(call.connectedAt, call.firstEchoFrameAt,
        'timing.firstEchoFrameAfterConnectedMs'),
      firstEchoFrameMethod: call.firstEchoFrameMethod,
    },
    answer: call.answer,
    testSource: source === 'test'
      ? { audioContextState: testSource.audioContext ? testSource.audioContext.state : UNVERIFIED }
      : null,
    notes: {
      srtpDecryptionFailures: 'not exposed by the browser\'s getStats',
      timing: 'milliseconds since Start; firstEchoFrame is the first echoed video frame shown',
    },
    unverified,
  };

  window.relaisStats = stats;
  els.stats.textContent = JSON.stringify(stats, null, 2);
  render(stats);
}

function render(stats) {
  const t = stats.transport || {};
  const rows = [
    ['Source', source === 'test' ? 'test pattern' : 'camera and microphone'],
    ['Call', call.state + (call.error ? ` (${call.error})` : '')],
    ['Connection', stats.connectionState || '-'],
    ['ICE', stats.iceConnectionState || '-'],
    ['DTLS', t.dtlsState ? `${t.dtlsState}, ${t.dtlsVersion}, ${t.dtlsCipher}` : '-'],
    ['SRTP', t.srtpCipher || '-'],
    ['Path', t.selectedCandidatePair ? `${t.selectedCandidatePair.local} → ${t.selectedCandidatePair.remote}` : '-'],
    ['Audio out / echo in', stats.outbound
      ? `${stats.outbound.audio.packetsSent} / ${stats.inbound.audio.packetsReceived} packets` : '-'],
    ['Video out / echo in', stats.outbound
      ? `${stats.outbound.video.framesEncoded} frames encoded / ${stats.inbound.video.framesDecoded} echoed frames decoded`
      : '-'],
    ['First echoed frame', stats.timing ? `${stats.timing.firstEchoFrameAfterConnectedMs} ms after connected` : '-'],
    ['SRTP decryption failures', UNVERIFIED],
  ];
  if (stats.testSource) {
    rows.push(['Test audio', stats.testSource.audioContextState]);
  }
  els.summary.replaceChildren(...rows.flatMap(([name, value]) => {
    const dt = document.createElement('dt');
    dt.textContent = name;
    const dd = document.createElement('dd');
    dd.textContent = String(value);
    if (value === 'connected' || value === 'running') {
      dd.className = 'ok';
    } else if (value === 'failed' || String(value).startsWith('failed')) {
      dd.className = 'bad';
    }
    return [dt, dd];
  }));
}

els.sourceLabel.textContent = source === 'test' ? 'a test pattern and tone' : 'your camera and microphone';
els.start.addEventListener('click', start);
els.hangup.addEventListener('click', hangup);
els.unmute.addEventListener('change', () => {
  els.remote.muted = !els.unmute.checked;
});
window.addEventListener('pagehide', () => {
  if (resourceURL) {
    fetch(resourceURL, { method: 'DELETE', keepalive: true }).catch(() => {});
  }
});

render(window.relaisStats);
if (autostart) {
  start();
}
