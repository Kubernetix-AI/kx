# Written by kx deploy for a Node app.
FROM node:22-alpine
WORKDIR /app
COPY package.json yarn.lock ./
RUN corepack enable && yarn install --frozen-lockfile
COPY . .
ENV PORT=8080 HOST=0.0.0.0
EXPOSE 8080
# Some hosts refuse containers that run as root, and can only check a numeric user.
USER 1000
CMD ["node","app.js"]
