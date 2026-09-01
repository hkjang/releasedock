import { Box, Divider, Typography } from '@mui/material';
import type { ReactNode } from 'react';

/**
 * Renders a small Markdown subset used by guide posts.
 *
 * Everything is built as React elements; no HTML from a post is ever
 * interpolated, so a post cannot inject markup even though administrators can
 * write arbitrary text.
 *
 * Supported: # ## ### headings, - bullets, 1. numbered lists, ``` fenced code,
 * > quotes, --- rules, **bold**, `code`, and blank-line separated paragraphs.
 */
export function GuideBody({ body }: { body: string }) {
  return <Box sx={{ '& > *:first-of-type': { mt: 0 } }}>{renderBlocks(body)}</Box>;
}

function renderBlocks(body: string): ReactNode[] {
  const lines = body.replace(/\r\n/g, '\n').split('\n');
  const blocks: ReactNode[] = [];
  let paragraph: string[] = [];
  let key = 0;

  const flushParagraph = () => {
    if (!paragraph.length) return;
    blocks.push(
      <Typography key={`p-${key++}`} sx={{ my: 1.5, lineHeight: 1.75 }}>
        {renderInline(paragraph.join(' '))}
      </Typography>,
    );
    paragraph = [];
  };

  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index];

    if (line.trim().startsWith('```')) {
      flushParagraph();
      const code: string[] = [];
      index += 1;
      while (index < lines.length && !lines[index].trim().startsWith('```')) {
        code.push(lines[index]);
        index += 1;
      }
      blocks.push(
        <Box
          key={`code-${key++}`}
          component="pre"
          sx={{
            my: 2, p: 2, borderRadius: 1, bgcolor: 'background.default',
            overflowX: 'auto', fontSize: 13, lineHeight: 1.6, m: 0,
          }}
        >
          {code.join('\n')}
        </Box>,
      );
      continue;
    }

    if (!line.trim()) {
      flushParagraph();
      continue;
    }

    if (/^---+$/.test(line.trim())) {
      flushParagraph();
      blocks.push(<Divider key={`hr-${key++}`} sx={{ my: 3 }} />);
      continue;
    }

    const heading = /^(#{1,4})\s+(.*)$/.exec(line);
    if (heading) {
      flushParagraph();
      const level = heading[1].length;
      blocks.push(
        <Typography
          key={`h-${key++}`}
          variant={level === 1 ? 'h2' : level === 2 ? 'h3' : 'h4'}
          sx={{ mt: level === 1 ? 0 : 3.5, mb: 1.25 }}
        >
          {renderInline(heading[2])}
        </Typography>,
      );
      continue;
    }

    if (line.trim().startsWith('> ')) {
      flushParagraph();
      const quote: string[] = [];
      while (index < lines.length && lines[index].trim().startsWith('> ')) {
        quote.push(lines[index].trim().slice(2));
        index += 1;
      }
      index -= 1;
      blocks.push(
        <Box
          key={`q-${key++}`}
          sx={{ my: 2, pl: 2, borderLeft: '3px solid', borderColor: 'primary.main', color: 'text.secondary' }}
        >
          <Typography sx={{ lineHeight: 1.7 }}>{renderInline(quote.join(' '))}</Typography>
        </Box>,
      );
      continue;
    }

    const bullet = /^\s*[-*]\s+(.*)$/.exec(line);
    const numbered = /^\s*\d+\.\s+(.*)$/.exec(line);
    if (bullet || numbered) {
      flushParagraph();
      const ordered = Boolean(numbered);
      const items: string[] = [];
      while (index < lines.length) {
        const candidate = ordered
          ? /^\s*\d+\.\s+(.*)$/.exec(lines[index])
          : /^\s*[-*]\s+(.*)$/.exec(lines[index]);
        if (!candidate) break;
        items.push(candidate[1]);
        index += 1;
      }
      index -= 1;
      blocks.push(
        <Box
          key={`l-${key++}`}
          component={ordered ? 'ol' : 'ul'}
          sx={{ my: 1.5, pl: 3, '& li': { mb: 0.75, lineHeight: 1.7 } }}
        >
          {items.map((item, itemIndex) => (
            <li key={itemIndex}>{renderInline(item)}</li>
          ))}
        </Box>,
      );
      continue;
    }

    paragraph.push(line.trim());
  }
  flushParagraph();
  return blocks;
}

// Inline formatting is tokenised rather than replaced into HTML, so the output
// is always plain text inside React elements.
function renderInline(text: string): ReactNode[] {
  const nodes: ReactNode[] = [];
  const pattern = /(\*\*[^*]+\*\*|`[^`]+`)/g;
  let lastIndex = 0;
  let key = 0;
  for (let match = pattern.exec(text); match; match = pattern.exec(text)) {
    if (match.index > lastIndex) nodes.push(text.slice(lastIndex, match.index));
    const token = match[0];
    if (token.startsWith('**')) {
      nodes.push(<strong key={key++}>{token.slice(2, -2)}</strong>);
    } else {
      nodes.push(
        <Box
          key={key++}
          component="code"
          sx={{ px: 0.6, py: 0.15, borderRadius: 0.5, bgcolor: 'action.selected', fontSize: '0.9em' }}
        >
          {token.slice(1, -1)}
        </Box>,
      );
    }
    lastIndex = match.index + token.length;
  }
  if (lastIndex < text.length) nodes.push(text.slice(lastIndex));
  return nodes;
}
