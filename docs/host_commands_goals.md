You are an experienced software architect. I want you to review this job builder located at

https://github.com/geraldthewes/jobforge

And in particular https://github.com/geraldthewes/jobforge/blob/main/README.md  and

https://github.com/geraldthewes/jobforge/blob/main/docs/JobSpec.md



And write a detailed spec for an AI agent to implement a new test feature

Today that builder support two testing mode

* Execute the command in the docker execcomand directive

* Execute specified commands inside the container being tested

For web services this is not convenient

I want to add a host based test options. We can configure it via

test.host_command

this would be mutually exclusive of the two others (an error should be reported if more than one test option is selected)

This command would be on executed by a shell on the client host where jobforge runs, and inherit the current environment, By default the shell is /bin/sh

If another shell is needed, it can be specified in test.host_shell

The container will need to have an open port. Since the purpose of this option is to test web servers (or servers that listen to a port), these servers listen to a fixed port inside the container, this port will be configured with test.server_port. When the test container is launched, the nomad job specification for it will need to map the internal port to a dynamic port in nomad.
Before launching the host_command, the jobforge program will need to set the ENDPOINT_ADDR to <nomad_server>:<nomad_port> set for that test container. 

Because the jobforge needs that adddress, this test option only works with the --watch option as that information needs to be fetched by jobforge. It is the jobforge cli tool that will fork/exec the host_command.

In addition to capturing the test container stdin and stdout, the stdin and stdout of the host command will also need to be captured by jobforge. That test needs to return success or failure as indicated by its exit status code. Like for the other test commands, the docker image is only publiched if the test succeeds.

Please comment on these requirements, any changes you think are importand and if you agree please write a requirements document for an AI coding agent.
