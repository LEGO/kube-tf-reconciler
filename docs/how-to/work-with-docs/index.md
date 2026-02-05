---
title: "Working with our documentation"
---
# Documentation

You will find the documentation here.

Find the Architecture Decision Records in the `./adr` folder
 
## Rendering
We use [Hugo](https://gohugo.io/) for page generation, we keep the configuration in the `/.pages` folder and then we
have our documentation here as Markdown.

You can [install Hugo](https://gohugo.io/getting-started/installing/) with brew `brew install hugo` and then run the 
following commands:

```bash
# Fetch themes
git submodule update --init --recursive
# Run server
hugo server --source .pages
```

And then it should run a website on port 1313.

Hugo allows for regular Markdown documents to be turned into a website, for example this document can be found at 
[localhost:1313/](http://localhost:1313/).

## Formatting documents for Hugo
Hugo introduces two "special" constructs in Markdown. [**Shortcodes**](https://gohugo.io/content-management/shortcodes/) 
and [**Front Matter**](https://gohugo.io/content-management/front-matter/).

### File structure
Each Markdown document becomes a webpage available at the corresponding path. Each folder becomes a "top-level" page 
with an `_index.md` to control the contents. The theme describes it [here](https://mcshelby.github.io/hugo-theme-relearn/cont/pages/).

### Front Matter
A "Front Matter" is simply some YAML-based metadata about a page enclosed in `---`, for most pages all you need to 
specify is a title for example:

```markdown
---
title: "Example"
---

Some Text Here
```

However, the Theme we are using has a special ["Chapter" construct](https://mcshelby.github.io/hugo-theme-relearn/cont/archetypes/index.html#archetypes-chapter) 
that are suited for top-level pages with sub-pages beneath it.

```markdown
---
title: "Example"
chapter: true
pre: "<b>4. Examples</b>
---

Some Text Here
```

### Shortcodes

In Hugo a Shortcode is a piece of logic you can inject: 
```markdown
Some text {{</* gist spf13 7896402 */>}}
```

The "logic" is simply a templated HTML snippet that can use Hugo's more expressive functionality. We can create our own 
of these if we want to, but [Hugo](https://gohugo.io/content-management/shortcodes/#use-hugos-built-in-shortcodes) and
the [theme](https://mcshelby.github.io/hugo-theme-relearn/shortcodes/) both have a good selection available.

### Referencing images
All files in the `docs` folder will be available as static files, so if you add an image at `/docs/example.png` you can
reference it in Markdown as `![example](/example.png)`.

### Diagrams
Our theme [supports Mermaid](https://mcshelby.github.io/hugo-theme-relearn/shortcodes/mermaid/) out-of-the-box for small inline diagrams.

On top of that, we have created a custom ShortCode to render Draw.io diagrams:
```markdown
{{</* drawio src="example.drawio" */>}}
```
{{< drawio src="example.drawio" >}}

And one for PlantUML:
```markdown
{{</* plantuml src="example.puml" */>}}
```
{{< plantuml src="example.puml" >}}


## The theme
We are using [Relearn](https://mcshelby.github.io/hugo-theme-relearn) which has a lot of features and a very good documentation which not
only described the theme itself, but also more fundamental issues like [Markdown Syntax](https://mcshelby.github.io/hugo-theme-relearn/cont/markdown/).
