#!/usr/bin/env python3
"""kubectl exec credential plugin: a fresh GitHub Actions OIDC token for the
production cluster (audience gamemesh-k8s).

GitHub's tokens live five minutes, shorter than a slow rollout. Instead of a
fixed token in the kubeconfig, kubectl runs this whenever it needs one and
again once the returned token expires, so no wait can outlive its token.

The token goes only to stdout, which kubectl reads; never to the log. Each
fetch is noted on stderr (claims only) so a run shows how often it renewed.
With --claims it prints a fresh token's non-secret claims instead.
"""
import base64
import datetime
import json
import os
import sys
import urllib.parse
import urllib.request

AUDIENCE = "gamemesh-k8s"
# Hand the token back a little before it really expires, so a request is not
# sent with a token that dies in flight.
MARGIN_S = 30


def fetch():
    url = os.environ["ACTIONS_ID_TOKEN_REQUEST_URL"] + "&audience=" + urllib.parse.quote(AUDIENCE)
    req = urllib.request.Request(
        url, headers={"Authorization": "bearer " + os.environ["ACTIONS_ID_TOKEN_REQUEST_TOKEN"]}
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        token = json.load(resp)["value"]
    payload = token.split(".")[1]
    claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
    return token, claims


def main():
    token, claims = fetch()
    if "--claims" in sys.argv[1:]:
        keep = ("sub", "aud", "ref", "environment", "repository_id", "repository_owner_id")
        out = {k: claims.get(k) for k in keep}
        out["lifetime_s"] = claims["exp"] - claims["iat"]
        print(json.dumps(out, indent=1))
        return
    expires = datetime.datetime.fromtimestamp(claims["exp"] - MARGIN_S, datetime.timezone.utc)
    print(
        f"gh-oidc-token: new cluster token, valid until {expires:%H:%M:%S} UTC",
        file=sys.stderr,
    )
    json.dump(
        {
            "apiVersion": "client.authentication.k8s.io/v1",
            "kind": "ExecCredential",
            "status": {
                "token": token,
                "expirationTimestamp": expires.strftime("%Y-%m-%dT%H:%M:%SZ"),
            },
        },
        sys.stdout,
    )


if __name__ == "__main__":
    main()
