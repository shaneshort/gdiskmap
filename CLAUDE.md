## Rules. DO NOT break these rules.
- Be brutally honest, don't be a yes man.
- If I am wrong, point it out bluntly.
- I need honest feedback on my code.
- If I am not following best practices, tell me.
- If you're not sure or don't understand something, ALWAYS ask for clarification.
- Ensure comprehensive commenting and documentation, especially for complex logic-- and avoid the use of hyperbolic language.
- Ensure that all code is properly formatted and adheres to the project's style guide, if present-- else use a consistent style.
- Make sure to fix problems properly, not just superficially.
- Consistency is key, inspect what's already been done and think carefully before suggesting consistent options.
- follow the 'Principal of the least surprise'-- don't surprise anyone with unexpected behaviour. Implement one, obvious way to do things.
- Don't create "temp files" when testing or debugging, use proper logging instead.
- Leverage existing tools and libraries where possible, don't reinvent the wheel.
- Keep things simple. If a solution looks complex, let's evaluate if it can be simplified.
- Mark any unfinished or questionable code with `TODO` comments and explain why it is there.
- If you find any TODOs, address them or explain why they are still necessary.
- When updating documentation, treat it as the current state-- don't talk about improvements or plans unless explicitly stated.
- When writing comments-- ensure you include the article when referencing, i.e. 'the' or 'a'.
- Ensure all code is written to Australian/British English. it's 'colour' not 'color'.
- Think critically about what you're doing-- if it doesn't seem like a good idea or if it feels like a shortcut, don't do it.
- When doing any unit testing etc, redirect the output to a temp file then grep for your results, so you don't have to run it a second time to see the full output.
- Ensure all code generated is run through the appropriate language linters to ensure good quality code is being generated.

Project Information:
Read the README.md file for project details, If it exists.
Read REFACTOR.md for any refactoring guidelines, if it exists

When making changes, ensure the following:
- Document any new features or changes in the README.md file. As well as writing any feature specific documentation into doc/ (updating any existing ones if required)
- Use meaningful commit messages that clearly describe the changes made.

If there are any useful tools, resources or information I provide, update this file accordingly.

## Go project conventions

- Follow the Google Go style guide, especially the best practices:
  https://google.github.io/styleguide/go/best-practices.html
- Module path: `github.com/shaneshort/gdiskmap` (repository https://github.com/shaneshort/gdiskmap.git).
- Go 1.25 (keep `go.mod` at 1.25; `golang.org/x/sys` is pinned to a release that supports it).
- `make test` runs the tests; `make lint` runs golangci-lint for the host and for Linux
  (the ioctl code only builds on Linux). `make build` produces static Linux binaries in `bin/`.
- Test output: `go test ./... > <scratch>/test.log 2>&1` then grep the log.

## Reference material

- MPI message layouts and constants: the Linux kernel's `drivers/scsi/mpt3sas/mpi/`
  (`mpi2.h`, `mpi2_cnfg.h`, `mpi2_init.h`) and the ctl ioctl in `drivers/scsi/mpt3sas/mpt3sas_ctl.[ch]`.
- `zpool status -j` JSON format: zpool-status(8) in the OpenZFS repository.
- Reference host `pve`: Proxmox, 3x LSI SAS2 HBAs (SAS2008, 2x SAS2308/SAS9207-8i), direct-attach
  SGPIO backplanes, ZFS 2.4. Use `gdiskmap dump` there to capture test fixtures.
