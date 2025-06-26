## Contribution

This is great that you'd like to contribute to this project. All change requests should go through the steps described below.

## Pull Requests

**Please, make sure you open an issue before starting with a Pull Request, unless it's a typo or an obvious error.** Pull requests are the best way to propose changes to the specification.

## Conventional commits

Our repository follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/#summary) specification. Releasing to GitHub and NPM is done with the support of [semantic-release](https://semantic-release.gitbook.io/semantic-release/).

Pull requests should have a title that follows the specification, otherwise, merging is blocked. If you are not familiar with the specification, simply ask maintainers to modify. You can also use this cheat sheet if you want:

- `fix: ` prefix in the title indicates that PR is a bug fix and a PATCH release must be triggered.
- `feat: ` prefix in the title indicates that PR is a feature and a MINOR release must be triggered.
- `docs: ` prefix in the title indicates that PR is only related to the documentation and there is no need to trigger a release.
- `chore: ` prefix in the title indicates that PR is only related to cleanup in the project and there is no need to trigger a release.
- `test: ` prefix in the title indicates that PR is only related to tests and there is no need to trigger a release.
- `refactor: ` prefix in the title indicates that PR is only related to refactoring and there is no need to trigger a release.

What about a MAJOR release? Just add `!` to the prefix, like `fix!: ` or `refactor!: `

Prefix that follows specification is not enough though. Remember that the title must be clear and descriptive with the usage of [imperative mood](https://chris.beams.io/posts/git-commit/#imperative).

## Resources

- [How to Contribute to Open Source](https://opensource.guide/how-to-contribute/)
- [Using Pull Requests](https://help.github.com/articles/about-pull-requests/)
- [GitHub Help](https://help.github.com)