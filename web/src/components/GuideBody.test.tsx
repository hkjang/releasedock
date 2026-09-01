import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { GuideBody } from './GuideBody';

describe('GuideBody', () => {
  it('renders headings, lists and code blocks', () => {
    const { container } = render(
      <GuideBody
        body={[
          '# 심플 모드로 배포하기',
          '',
          '## 절차',
          '',
          '1. 로그인합니다',
          '2. 파일을 올립니다',
          '',
          '- 첫 항목',
          '- 둘째 항목',
          '',
          '```',
          'docker load --input app.tar.gz',
          '```',
        ].join('\n')}
      />,
    );
    expect(screen.getByText('심플 모드로 배포하기')).toBeInTheDocument();
    expect(screen.getByText('절차')).toBeInTheDocument();
    expect(container.querySelectorAll('ol li')).toHaveLength(2);
    expect(container.querySelectorAll('ul li')).toHaveLength(2);
    expect(container.querySelector('pre')?.textContent).toBe('docker load --input app.tar.gz');
  });

  it('renders inline bold and code', () => {
    const { container } = render(<GuideBody body={'앞 **굵게** 뒤 `코드` 끝'} />);
    expect(container.querySelector('strong')?.textContent).toBe('굵게');
    expect(container.querySelector('code')?.textContent).toBe('코드');
  });

  // Administrators can write anything into a post, so markup must never be
  // interpreted as HTML.
  it('never interprets HTML from a post', () => {
    const { container } = render(
      <GuideBody body={'<script>alert(1)</script> 그리고 <img src=x onerror=alert(2)> 입니다'} />,
    );
    expect(container.querySelector('script')).toBeNull();
    expect(container.querySelector('img')).toBeNull();
    expect(container.textContent).toContain('<script>alert(1)</script>');
  });

  it('keeps blank-line separated paragraphs apart', () => {
    const { container } = render(<GuideBody body={'첫 문단입니다.\n\n둘째 문단입니다.'} />);
    const paragraphs = [...container.querySelectorAll('p')].map((node) => node.textContent);
    expect(paragraphs).toEqual(['첫 문단입니다.', '둘째 문단입니다.']);
  });

  it('renders quotes and horizontal rules', () => {
    const { container } = render(<GuideBody body={'> 참고할 내용\n\n---\n\n본문'} />);
    expect(container.textContent).toContain('참고할 내용');
    expect(container.querySelector('hr')).not.toBeNull();
  });

  it('renders an empty body without throwing', () => {
    const { container } = render(<GuideBody body="" />);
    expect(container.textContent).toBe('');
  });
});
