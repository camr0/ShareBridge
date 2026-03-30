let pc, ws;
let pendingCandidates = [];
let remoteDescSet = false;

function status(msg) {
  document.getElementById('status').textContent = msg;
}

function join() {
  const code = document.getElementById('code').value.trim();
  if (!code) return;
  status('Connecting...');

  ws = new WebSocket(`ws://${location.host}/ws/client?session=${code}`);

  ws.onmessage = async (event) => {
    const msg = JSON.parse(event.data);

    switch (msg.type) {
      case 'ice_config':
        pc = new RTCPeerConnection({ iceServers: msg.ice_servers });

        pc.onicecandidate = (e) => {
          if (e.candidate) {
            ws.send(JSON.stringify({
              type: 'ice_candidate',
              candidate: e.candidate.toJSON(),
            }));
          }
        };

        pc.ondatachannel = (e) => {
          e.channel.onopen = () => {
            status('✓ DataChannel open!');
            console.log('DataChannel open');
          };
        };

        pc.onconnectionstatechange = () => {
          if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
            status('Connection lost');
          }
        };
        break;

      case 'offer':
        await pc.setRemoteDescription({ type: 'offer', sdp: msg.sdp });
        remoteDescSet = true;

        // Flush any candidates that arrived before the offer
        for (const c of pendingCandidates) {
          await pc.addIceCandidate(c);
        }
        pendingCandidates = [];

        const answer = await pc.createAnswer();
        await pc.setLocalDescription(answer);
        ws.send(JSON.stringify({ type: 'answer', sdp: answer.sdp }));
        status('Negotiating...');
        break;

      case 'ice_candidate':
        if (!remoteDescSet) {
          pendingCandidates.push(msg.candidate);
        } else {
          await pc.addIceCandidate(msg.candidate);
        }
        break;

      case 'error':
        status('Error: ' + msg.message);
        break;
    }
  };

  ws.onerror = () => status('WebSocket error');
  ws.onclose = () => { if (pc) pc.close(); };
}
