# Written by kx deploy for a Node app.
FROM node:22-alpine
WORKDIR /app
ENV COREPACK_HOME=/usr/local/share/corepack COREPACK_ENABLE_DOWNLOAD_PROMPT=0
COPY package.json yarn.lock ./
RUN corepack enable && yarn install --frozen-lockfile
COPY . .
ENV PORT=8080 HOST=0.0.0.0
# npm keeps its cache and logs in /tmp, so the app runs as any user.
ENV NPM_CONFIG_CACHE=/tmp/.npm NPM_CONFIG_UPDATE_NOTIFIER=false
EXPOSE 8080
# Some hosts refuse containers that run as root, and can only check a numeric user.
USER 1000
CMD ["node","app.js"]
