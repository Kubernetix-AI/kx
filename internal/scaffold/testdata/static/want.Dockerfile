# Written by kx deploy for a static site.
FROM nginxinc/nginx-unprivileged:1.27-alpine
COPY . /usr/share/nginx/html
EXPOSE 8080
# Some hosts refuse containers that run as root, and can only check a numeric user.
USER 101
