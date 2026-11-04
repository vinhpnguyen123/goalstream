// The gs_invite check, in nginx (M9). The same rules as internal/auth.VerifyInvite — keep the
// two in step: value = base64url(id|exp|hex HMAC-SHA256(INVITE_SECRET, id|exp)); valid iff the
// MAC matches (constant time), exp is in the future, and id is still in INVITE_CODES (removing
// an id revokes every cookie it issued). Returns "1" or "0"; never says which check failed.
import crypto from 'crypto';

function verify(r) {
    const secret = process.env.INVITE_SECRET || '';
    const cookie = r.variables.cookie_gs_invite || '';
    // Fail closed on a missing secret (config requires ≥ 32 bytes); strict alphabet, as Go's
    // RawURLEncoding (Buffer would skip characters it does not know).
    if (secret.length < 32 || !/^[A-Za-z0-9_-]+$/.test(cookie)) {
        return '0';
    }
    const parts = Buffer.from(cookie, 'base64url').toString().split('|');
    if (parts.length !== 3) {
        return '0';
    }
    const id = parts[0], exp = parts[1], sig = parts[2];
    const want = crypto.createHmac('sha256', secret).update(id + '|' + exp).digest('hex');
    if (!equal(sig, want)) {
        return '0';
    }
    if (!/^-?[0-9]+$/.test(exp) || Date.now() / 1000 >= Number(exp)) {
        return '0';
    }
    return ids().indexOf(id) >= 0 ? '1' : '0';
}

// equal compares in time that depends only on the lengths (the MAC is always 64 hex chars).
function equal(a, b) {
    if (a.length !== b.length) {
        return false;
    }
    let d = 0;
    for (let i = 0; i < a.length; i++) {
        d |= a.charCodeAt(i) ^ b.charCodeAt(i);
    }
    return d === 0;
}

// ids is the id half of INVITE_CODES (id:code,id:code); the codes themselves are not needed.
function ids() {
    return (process.env.INVITE_CODES || '').split(',').map(function (pair) {
        return pair.split(':')[0].trim();
    }).filter(function (id) { return id !== ''; });
}

export default { verify };
