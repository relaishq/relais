## FEATURE:

- The aim of relais is to make a easy scalable and extensible media server.
- By adding more egress runner, we can support more viewer
- By adding more ingress runner, we can support more input
- By adding transform runner, we can convert data that saved by ingress runner and output can be consume by engress runner
- Instead of save data in memory and the main different for relais is it save the data in some common storage layer. such as redis.
- it will add delay by using storage layer to save data. but we are not aiming the communcation level of delay.
- our solution aim more on 1-to-many live stream and offer a decent delay
- and runners are stateless. so they can be add and remove easily. not totally stateless, it will still need to manage rtmp connection for example. but as soon as the connection is done. it's easy to tear down. add adding more runners into the system is effortless. 
- This repo is just started. let's make a plan.md. our final goal is to provide a super scalable solution to that easy to deploy and manange.

## DOCUMENTATION:

TUI for manage runners and system status: https://github.com/charmbracelet/bubbles

## OTHER CONSIDERATIONS:

- Include critical implementation details in the PRPs
- Ultrathink on the architecture
- I prefer monorepo style if applicable