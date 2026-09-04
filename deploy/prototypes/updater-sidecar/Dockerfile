# PROTOTYPE (#113) — throwaway updater sidecar image. Not production code.
# docker:cli ships the docker CLI + compose plugin on alpine; python3 runs updater.py.
FROM docker:29-cli
RUN apk add --no-cache python3 curl
COPY updater.py /usr/local/bin/updater.py
ENTRYPOINT ["python3", "/usr/local/bin/updater.py"]
