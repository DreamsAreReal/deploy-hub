#!/usr/bin/env python3
"""compose-main-dockerfile — print the Dockerfile path of the main service.

CI must build the MAIN (built) service's image. When the developer's compose
puts that service's build context in a subdirectory (e.g. build: ./api or a
long-form build with context/dockerfile), CI needs to know which Dockerfile to
build. This resolves it from the same "main service" rule as compose-render.

    compose-main-dockerfile.py <app> <in_compose>   # prints a path like ./api/Dockerfile

Falls back to ./Dockerfile when the main service has no explicit build context.
"""
import importlib.util
import os
import sys

try:
    import yaml
except ImportError:
    print("Dockerfile")
    sys.exit(0)

# reuse the exact main-service selection from the renderer (loaded by path,
# since the module file name contains a hyphen and is not importable by name)
_here = os.path.dirname(os.path.abspath(__file__))
_spec = importlib.util.spec_from_file_location(
    "compose_render", os.path.join(_here, "compose-render.py"))
_cr = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_cr)
pick_main = _cr.pick_main


def main():
    if len(sys.argv) != 3:
        print("Dockerfile")
        return
    _app, in_path = sys.argv[1], sys.argv[2]
    try:
        with open(in_path, encoding="utf-8") as fh:
            doc = yaml.safe_load(fh) or {}
    except OSError:
        print("Dockerfile")
        return
    services = doc.get("services") or {}
    try:
        name = pick_main(services)
    except ValueError:
        print("Dockerfile")
        return
    build = services[name].get("build")
    if build is None:
        print("Dockerfile")
        return
    if isinstance(build, str):
        # build: ./api  => ./api/Dockerfile
        print(os.path.join(build, "Dockerfile"))
        return
    if isinstance(build, dict):
        ctx = build.get("context", ".")
        df = build.get("dockerfile", "Dockerfile")
        print(os.path.join(ctx, df))
        return
    print("Dockerfile")


if __name__ == "__main__":
    main()
