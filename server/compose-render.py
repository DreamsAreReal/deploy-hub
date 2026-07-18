#!/usr/bin/env python3
"""compose-render — turn a repo's docker-compose.yml into a deploy-ready one.

This is the core of "bring your own compose" (WV6). The developer declares
ports / health / env / volumes / sidecars in their own compose; the system runs
THAT, never a template. Building an image on the 1-CPU VPS is forbidden, so the
main service's `build:` is replaced with the CI-built GHCR image.

    compose-render.py <app> <in_compose> <out_compose>

Steps:
  * pick the main service (a `build:` key, or `deploy-hub.main: "true"`, or the
    only service) and replace its build with `image: ${DEPLOY_IMAGE:?...}` and
    a stable container_name = <app>. Using the DEPLOY_IMAGE placeholder (instead
    of a baked-in :sha) keeps the rendered file reusable: the runner substitutes
    the concrete image:tag at up-time, so rollback just re-runs with the previous
    tag — exactly like the template path.
  * sidecars (postgres/redis/...) are left as-is and pulled from their registries
  * apply SAFE DEFAULTS only where the developer did not set them: mem_limit,
    json-file log rotation, restart: unless-stopped
  * attach env_file: .env to the main service (secrets come from /opt/<app>/.env)
  * decide the public port + health mode and print them as key=value lines to
    stdout so the runner/CI can consume them without re-parsing yaml

Labels honored on the main service:
  * deploy-hub.main: "true"    force this service to be the main one
  * deploy-hub.public: "false" the app has no public URL (bot-like)
  * deploy-hub.host: "true"    this service's published port is the one to expose

Only PyYAML is required (present on the VPS and on ubuntu-latest runners).
Emits nothing to the image; reads no secrets.
"""
import copy
import sys

try:
    import yaml
except ImportError:
    sys.stderr.write("compose-render: PyYAML is required\n")
    sys.exit(3)

# safe defaults layered ONLY where the developer left them unset
DEFAULT_MEM_LIMIT = "256m"
DEFAULT_LOGGING = {"driver": "json-file",
                   "options": {"max-size": "10m", "max-file": "3"}}
DEFAULT_RESTART = "unless-stopped"


def truthy(v):
    return str(v).strip().lower() in ("true", "1", "yes")


def labels_of(svc):
    """Return service labels as a dict (compose allows list or map form)."""
    labels = svc.get("labels", {})
    if isinstance(labels, list):
        out = {}
        for item in labels:
            k, _, v = str(item).partition("=")
            out[k.strip()] = v.strip()
        return out
    if isinstance(labels, dict):
        return {str(k): str(v) for k, v in labels.items()}
    return {}


def pick_main(services):
    """Choose the main (built) service. Precedence: explicit label > build key >
    the sole service. Ambiguity is an error rather than a guess."""
    explicit = [n for n, s in services.items()
                if truthy(labels_of(s).get("deploy-hub.main", ""))]
    if len(explicit) == 1:
        return explicit[0]
    if len(explicit) > 1:
        raise ValueError("more than one service labelled deploy-hub.main")
    built = [n for n, s in services.items() if "build" in s]
    if len(built) == 1:
        return built[0]
    if len(built) > 1:
        raise ValueError("more than one service has build: — mark the main one "
                         "with label deploy-hub.main: \"true\"")
    if len(services) == 1:
        return next(iter(services))
    raise ValueError("cannot determine the main service — add build: or the "
                     "label deploy-hub.main: \"true\" to one service")


def published_port(svc):
    """Return the host port published by a service, or None. Handles the short
    forms '8000', '8000:8000', '127.0.0.1:9001:80' and the long dict form."""
    ports = svc.get("ports", [])
    for p in ports:
        if isinstance(p, dict):
            pub = p.get("published")
            if pub:
                return str(pub)
            continue
        parts = str(p).split(":")
        # [host:]container  or  ip:host:container
        if len(parts) == 1:
            return parts[0]
        if len(parts) == 2:
            return parts[0]
        if len(parts) >= 3:
            return parts[-2]
    return None


def die(msg):
    # V5: a clean one-line reason for the operator instead of a raw traceback
    sys.stderr.write(f"compose-render: {msg}\n")
    sys.exit(1)


def main():
    if len(sys.argv) != 4:
        sys.stderr.write(__doc__)
        sys.exit(2)
    app, in_path, out_path = sys.argv[1:4]

    # V5: bad YAML / wrong shape must produce a clear message, not a traceback
    try:
        with open(in_path, encoding="utf-8") as fh:
            doc = yaml.safe_load(fh) or {}
    except (OSError, yaml.YAMLError) as e:
        die(f"cannot parse compose: {e}")
    if not isinstance(doc, dict):
        die("compose is not a mapping (expected top-level services:)")
    services = doc.get("services")
    if not isinstance(services, dict) or not services:
        die("no services in compose (services: must be a mapping of names)")
    for name, svc in services.items():
        if not isinstance(svc, dict):
            die(f"service '{name}' is not a mapping")

    try:
        main_name = pick_main(services)
    except ValueError as e:
        die(str(e))
    main_svc = services[main_name]
    lbls = labels_of(main_svc)

    # 1) never build on the server: drop build:, point the main service at the
    # runner-provided image via the DEPLOY_IMAGE placeholder, and pin a stable
    # container_name so the runner can address it for health/status/logs
    main_svc.pop("build", None)
    main_svc["image"] = "${DEPLOY_IMAGE:?set by deploy runner}"
    main_svc["container_name"] = app

    # 2) attach the app's secrets file to the main service (idempotent)
    ef = main_svc.get("env_file")
    files = ef if isinstance(ef, list) else ([ef] if ef else [])
    if ".env" not in files:
        files.append(".env")
    main_svc["env_file"] = files

    # 3) safe defaults layered on EVERY service, only where unset (never override)
    for svc in services.values():
        svc.setdefault("restart", DEFAULT_RESTART)
        if "mem_limit" not in svc and "deploy" not in svc:
            svc["mem_limit"] = DEFAULT_MEM_LIMIT
        if "logging" not in svc:
            # deepcopy so each service gets an independent block (no YAML anchor)
            svc["logging"] = copy.deepcopy(DEFAULT_LOGGING)

    # 4) decide the public port + health mode for the runner
    public = "true"
    if not truthy(lbls.get("deploy-hub.public", "true")):
        public = "false"
    # host port: an explicit deploy-hub.host service wins, else the main service
    host_svc = main_svc
    for s in services.values():
        if truthy(labels_of(s).get("deploy-hub.host", "")):
            host_svc = s
            break
    # V3: published_port may be None (no ports: section). Coerce to "" so a valid
    # public app without a published port deploys as a no-URL/process app instead
    # of emitting `port=None`, which the runner would reject.
    port = (published_port(host_svc) or "") if public == "true" else ""
    health = "compose" if "healthcheck" in main_svc else ("http" if port else "process")

    with open(out_path, "w", encoding="utf-8") as fh:
        yaml.safe_dump(doc, fh, default_flow_style=False, sort_keys=False)

    # metadata for the caller (key=value, easy to eval in shell / read in python)
    print(f"main={main_name}")
    print(f"port={port}")
    print(f"public={public}")
    print(f"health={health}")


if __name__ == "__main__":
    main()
