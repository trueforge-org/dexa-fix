// Passkeys (WebAuthn): add one in Settings and sign in with one. The server sends the options as
// JSON; binary values travel as base64url. Nothing here runs without the browser API, and the
// elements it controls stay hidden, so the page works as before without it.
(function () {
  'use strict';
  if (!window.PublicKeyCredential || !navigator.credentials) {
    return;
  }

  function decode(s) {
    s = s.replace(/-/g, '+').replace(/_/g, '/');
    while (s.length % 4) { s += '='; }
    var bin = atob(s), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) { out[i] = bin.charCodeAt(i); }
    return out.buffer;
  }
  function encode(buf) {
    var b = new Uint8Array(buf), s = '';
    for (var i = 0; i < b.length; i++) { s += String.fromCharCode(b[i]); }
    return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }
  function post(url, body, asJSON) {
    return fetch(url, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': asJSON ? 'application/json' : 'application/x-www-form-urlencoded' },
      body: body
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (data) {
        if (!r.ok) { throw new Error(data.error || 'Something went wrong.'); }
        return data;
      });
    });
  }
  function show(msg) {
    var el = document.getElementById('passkey-msg');
    if (el) { el.textContent = msg || ''; }
  }
  function describe(err) {
    if (err && (err.name === 'NotAllowedError' || err.name === 'AbortError')) {
      return 'Cancelled, or the device did not respond. Try again.';
    }
    return (err && err.message) || 'Something went wrong.';
  }

  // Adding a passkey (Settings).
  var add = document.getElementById('passkey-add');
  if (add) {
    add.hidden = false;
    add.addEventListener('submit', function (e) {
      e.preventDefault();
      show('');
      post(add.dataset.begin, new URLSearchParams(new FormData(add)).toString(), false).then(function (data) {
        var pk = data.publicKey;
        pk.challenge = decode(pk.challenge);
        pk.user.id = decode(pk.user.id);
        (pk.excludeCredentials || []).forEach(function (c) { c.id = decode(c.id); });
        return navigator.credentials.create({ publicKey: pk });
      }).then(function (cred) {
        var payload = {
          id: cred.id, rawId: encode(cred.rawId), type: cred.type,
          authenticatorAttachment: cred.authenticatorAttachment,
          response: {
            attestationObject: encode(cred.response.attestationObject),
            clientDataJSON: encode(cred.response.clientDataJSON),
            transports: cred.response.getTransports ? cred.response.getTransports() : []
          },
          clientExtensionResults: cred.getClientExtensionResults()
        };
        return post(add.dataset.finish, JSON.stringify(payload), true);
      }).then(function (data) {
        window.location.href = data.redirect;
      }).catch(function (err) { show(describe(err)); });
    });
  }

  // Signing in (sign-in page).
  var button = document.getElementById('passkey-login');
  if (!button) {
    return;
  }
  var pending = null;

  function signIn(conditional) {
    var controller = new AbortController();
    if (conditional) { pending = controller; }
    return post('/passkey/login/begin', '', false).then(function (data) {
      var pk = data.publicKey;
      pk.challenge = decode(pk.challenge);
      (pk.allowCredentials || []).forEach(function (c) { c.id = decode(c.id); });
      var opts = { publicKey: pk, signal: controller.signal };
      if (conditional) { opts.mediation = 'conditional'; }
      return navigator.credentials.get(opts);
    }).then(function (cred) {
      var r = cred.response;
      var payload = {
        id: cred.id, rawId: encode(cred.rawId), type: cred.type,
        authenticatorAttachment: cred.authenticatorAttachment,
        response: {
          authenticatorData: encode(r.authenticatorData), clientDataJSON: encode(r.clientDataJSON),
          signature: encode(r.signature), userHandle: r.userHandle ? encode(r.userHandle) : ''
        },
        clientExtensionResults: cred.getClientExtensionResults()
      };
      return post('/passkey/login/finish', JSON.stringify(payload), true);
    }).then(function (data) {
      window.location.href = data.redirect;
    });
  }

  button.hidden = false;
  button.addEventListener('click', function () {
    show('');
    if (pending) { pending.abort(); pending = null; }
    signIn(false).catch(function (err) { show(describe(err)); });
  });

  // Browsers that support it offer the passkey in the e-mail field's suggestions.
  if (PublicKeyCredential.isConditionalMediationAvailable) {
    PublicKeyCredential.isConditionalMediationAvailable().then(function (ok) {
      if (ok) { signIn(true).catch(function () { /* the user did not pick one */ }); }
    });
  }
})();
