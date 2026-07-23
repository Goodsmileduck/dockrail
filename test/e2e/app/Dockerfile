FROM busybox:1.36
ARG VER=dev
ARG HEALTHY=1
# /www/version always reflects the build tag so a cutover is a provable swap.
# /www/health exists only when HEALTHY=1; the :bad image omits it, so
# `curl -fsS /health` returns non-2xx and dockrail readiness never passes.
RUN mkdir -p /www \
 && printf '%s' "$VER" > /www/version \
 && if [ "$HEALTHY" = "1" ]; then printf 'ok' > /www/health; fi
EXPOSE 8080
CMD ["httpd", "-f", "-v", "-p", "8080", "-h", "/www"]
